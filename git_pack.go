package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

type uploadPackRequest struct {
	wants      []string
	haves      []string
	done       bool
	sideband   bool
	sideband64 bool
	responded  bool
}

func serveUploadPack(ctx context.Context, repo *nativeGitRepo, stdin io.Reader, stdout io.Writer) error {
	req, err := readUploadPackRequestWithResponses(stdin, stdout)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if len(req.wants) == 0 {
		return nil
	}
	if !req.responded {
		if err := writePktLineString(stdout, "NAK\n"); err != nil {
			return err
		}
	}
	hashes, err := repo.objectsForUploadPack(ctx, req.wants, req.haves)
	if err != nil {
		return err
	}
	pack, err := repo.encodePack(ctx, hashes)
	if err != nil {
		return err
	}
	if req.sideband || req.sideband64 {
		maxPayload := 1000
		if req.sideband64 {
			maxPayload = 65516
		}
		if err := writeSideband(stdout, 1, pack, maxPayload); err != nil {
			return err
		}
		return writePktFlush(stdout)
	}
	_, err = stdout.Write(pack)
	return err
}

func readUploadPackRequest(stdin io.Reader) (uploadPackRequest, error) {
	return readUploadPackRequestWithResponses(stdin, nil)
}

func readUploadPackRequestWithResponses(stdin io.Reader, stdout io.Writer) (uploadPackRequest, error) {
	r := bufio.NewReader(stdin)
	var req uploadPackRequest
	for {
		line, err := readPktLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) && len(req.wants) > 0 {
				return req, nil
			}
			return req, err
		}
		switch line.kind {
		case pktLineFlush, pktLineDelim, pktLineResponseEnd:
			if len(req.wants) > 0 && req.done {
				return req, nil
			}
			if len(req.wants) > 0 && stdout != nil {
				if err := writePktLineString(stdout, "NAK\n"); err != nil {
					return req, err
				}
				req.responded = true
			}
			continue
		case pktLineData:
		}
		text := strings.TrimSpace(string(line.data))
		switch {
		case strings.HasPrefix(text, "want "):
			fields := strings.Fields(text)
			if len(fields) >= 2 && isHexHash(fields[1]) {
				req.wants = append(req.wants, fields[1])
			}
			for _, cap := range fields[2:] {
				switch cap {
				case "side-band":
					req.sideband = true
				case "side-band-64k":
					req.sideband64 = true
				}
			}
		case strings.HasPrefix(text, "have "):
			fields := strings.Fields(text)
			if len(fields) >= 2 && isHexHash(fields[1]) {
				req.haves = append(req.haves, fields[1])
			}
		case text == "done":
			req.done = true
			return req, nil
		}
	}
}

func (r *nativeGitRepo) objectsForUploadPack(ctx context.Context, wants, haves []string) ([]string, error) {
	exclude := map[string]struct{}{}
	for _, have := range haves {
		if err := r.collectReachableObjects(ctx, have, exclude); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	include := map[string]struct{}{}
	for _, want := range wants {
		if err := r.collectReachableObjects(ctx, want, include); err != nil {
			return nil, err
		}
	}
	var hashes []string
	for hash := range include {
		if _, ok := exclude[hash]; !ok {
			hashes = append(hashes, hash)
		}
	}
	sort.Strings(hashes)
	return hashes, nil
}

func (r *nativeGitRepo) collectReachableObjects(ctx context.Context, hash string, seen map[string]struct{}) error {
	hash = strings.TrimSpace(hash)
	if !isHexHash(hash) {
		return nil
	}
	if _, ok := seen[hash]; ok {
		return nil
	}
	obj, err := r.object(ctx, hash)
	if err != nil {
		return err
	}
	seen[hash] = struct{}{}
	switch obj.typ {
	case gitObjectCommit:
		commit, err := parseCommit(hash, obj.data)
		if err != nil {
			return err
		}
		if err := r.collectReachableObjects(ctx, commit.tree, seen); err != nil {
			return err
		}
		for _, parent := range commit.parents {
			if err := r.collectReachableObjects(ctx, parent, seen); err != nil {
				return err
			}
		}
	case gitObjectTree:
		entries, err := r.treeEntries(ctx, hash)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := r.collectReachableObjects(ctx, entry.hash, seen); err != nil {
				return err
			}
		}
	case gitObjectTag:
		target, err := parseTagTarget(obj.data)
		if err != nil {
			return err
		}
		return r.collectReachableObjects(ctx, target, seen)
	}
	return nil
}

func (r *nativeGitRepo) encodePack(ctx context.Context, hashes []string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	if err := binary.Write(&buf, binary.BigEndian, uint32(2)); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.BigEndian, uint32(len(hashes))); err != nil {
		return nil, err
	}
	for _, hash := range hashes {
		obj, err := r.object(ctx, hash)
		if err != nil {
			return nil, err
		}
		if err := writePackObject(&buf, obj); err != nil {
			return nil, fmt.Errorf("write pack object %s: %w", hash, err)
		}
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

func writePackObject(w io.Writer, obj gitObject) error {
	typ := packObjectType(obj.typ)
	if typ == 0 {
		return fmt.Errorf("unsupported object type %q", obj.typ)
	}
	if err := writePackObjectHeader(w, typ, len(obj.data)); err != nil {
		return err
	}
	zw := zlib.NewWriter(w)
	if _, err := zw.Write(obj.data); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

func writePackObjectHeader(w io.Writer, typ int, size int) error {
	first := byte((typ & 0x7) << 4)
	first |= byte(size & 0x0f)
	size >>= 4
	if size > 0 {
		first |= 0x80
	}
	if _, err := w.Write([]byte{first}); err != nil {
		return err
	}
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		if _, err := w.Write([]byte{b}); err != nil {
			return err
		}
	}
	return nil
}

func packObjectType(typ string) int {
	switch typ {
	case gitObjectCommit:
		return 1
	case gitObjectTree:
		return 2
	case gitObjectBlob:
		return 3
	case gitObjectTag:
		return 4
	default:
		return 0
	}
}
