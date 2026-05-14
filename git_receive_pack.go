package main

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

type receivedPackObject struct {
	hash string
	typ  string
	data []byte
}

type packedObjectRecord struct {
	offset   int
	typ      int
	data     []byte
	baseOfs  int
	baseHash string
	hash     string
}

func ingestReceivePack(ctx context.Context, repo *nativeGitRepo, store writableGitRemoteStore, pack io.Reader) (map[string]gitObject, error) {
	data, err := io.ReadAll(pack)
	if err != nil {
		return nil, err
	}
	objects, err := decodePackObjectsWithBase(ctx, repo, data)
	if err != nil {
		return nil, err
	}
	written := map[string]gitObject{}
	for _, obj := range objects {
		if err := writeLooseObject(ctx, store, obj.typ, obj.data); err != nil {
			return nil, err
		}
		written[obj.hash] = gitObject{typ: obj.typ, data: obj.data}
	}
	return written, nil
}

func decodePackObjects(pack []byte) ([]receivedPackObject, error) {
	return decodePackObjectsWithBase(context.Background(), nil, pack)
}

func decodePackObjectsWithBase(ctx context.Context, repo *nativeGitRepo, pack []byte) ([]receivedPackObject, error) {
	if len(pack) < 32 || !bytes.Equal(pack[:4], []byte("PACK")) {
		return nil, errors.New("invalid pack file")
	}
	if got := sha1.Sum(pack[:len(pack)-20]); !bytes.Equal(got[:], pack[len(pack)-20:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	version := int(readUint32(pack[4:8]))
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(readUint32(pack[8:12]))
	pos := 12
	records := make([]packedObjectRecord, 0, count)
	byOffset := map[int]int{}
	byHash := map[string]int{}
	for i := 0; i < count; i++ {
		offset := pos
		typ, headerLen, err := parsePackObjectHeader(pack[pos:])
		if err != nil {
			return nil, err
		}
		pos += headerLen
		rec := packedObjectRecord{offset: offset, typ: typ}
		switch typ {
		case 1, 2, 3, 4:
			data, n, err := inflatePackEntry(pack[pos:])
			if err != nil {
				return nil, err
			}
			rec.data = data
			pos += n
			rec.hash = objectHash(packTypeName(typ), data)
			byHash[rec.hash] = len(records)
		case 6:
			base, n, err := parseOFSDeltaBase(pack[pos:], uint64(offset))
			if err != nil {
				return nil, err
			}
			pos += n
			data, z, err := inflatePackEntry(pack[pos:])
			if err != nil {
				return nil, err
			}
			rec.baseOfs = int(base)
			rec.data = data
			pos += z
		case 7:
			if len(pack[pos:]) < 20 {
				return nil, errors.New("truncated ref delta")
			}
			rec.baseHash = hex.EncodeToString(pack[pos : pos+20])
			pos += 20
			data, n, err := inflatePackEntry(pack[pos:])
			if err != nil {
				return nil, err
			}
			rec.data = data
			pos += n
		default:
			return nil, fmt.Errorf("unsupported pack object type %d", typ)
		}
		byOffset[offset] = len(records)
		records = append(records, rec)
	}
	if pos != len(pack)-20 {
		return nil, errors.New("pack object data does not end at checksum")
	}
	resolved := map[int]receivedPackObject{}
	var resolve func(int) (receivedPackObject, error)
	resolve = func(i int) (receivedPackObject, error) {
		if obj, ok := resolved[i]; ok {
			return obj, nil
		}
		rec := records[i]
		switch rec.typ {
		case 1, 2, 3, 4:
			obj := receivedPackObject{hash: rec.hash, typ: packTypeName(rec.typ), data: rec.data}
			resolved[i] = obj
			return obj, nil
		case 6:
			baseIndex, ok := byOffset[rec.baseOfs]
			if !ok {
				return receivedPackObject{}, fmt.Errorf("missing ofs-delta base at offset %d", rec.baseOfs)
			}
			base, err := resolve(baseIndex)
			if err != nil {
				return receivedPackObject{}, err
			}
			data, err := applyDelta(base.data, rec.data)
			if err != nil {
				return receivedPackObject{}, err
			}
			obj := receivedPackObject{typ: base.typ, data: data}
			obj.hash = objectHash(obj.typ, obj.data)
			resolved[i] = obj
			byHash[obj.hash] = i
			return obj, nil
		case 7:
			baseIndex, ok := byHash[rec.baseHash]
			var base receivedPackObject
			if ok {
				var err error
				base, err = resolve(baseIndex)
				if err != nil {
					return receivedPackObject{}, err
				}
			} else if repo != nil {
				obj, err := repo.object(ctx, rec.baseHash)
				if err != nil {
					return receivedPackObject{}, fmt.Errorf("missing ref-delta base %s", rec.baseHash)
				}
				base = receivedPackObject{hash: rec.baseHash, typ: obj.typ, data: obj.data}
			} else {
				return receivedPackObject{}, fmt.Errorf("missing ref-delta base %s", rec.baseHash)
			}
			data, err := applyDelta(base.data, rec.data)
			if err != nil {
				return receivedPackObject{}, err
			}
			obj := receivedPackObject{typ: base.typ, data: data}
			obj.hash = objectHash(obj.typ, obj.data)
			resolved[i] = obj
			byHash[obj.hash] = i
			return obj, nil
		default:
			return receivedPackObject{}, fmt.Errorf("unsupported pack object type %d", rec.typ)
		}
	}
	var objects []receivedPackObject
	for i := range records {
		obj, err := resolve(i)
		if err != nil {
			return nil, err
		}
		objects = append(objects, obj)
	}
	return objects, nil
}

func inflatePackEntry(data []byte) ([]byte, int, error) {
	src := bytes.NewReader(data)
	reader, err := zlib.NewReader(src)
	if err != nil {
		return nil, 0, err
	}
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		_ = reader.Close()
		return nil, 0, err
	}
	if err := reader.Close(); err != nil {
		return nil, 0, err
	}
	consumed := len(data) - src.Len()
	return out.Bytes(), consumed, nil
}

func writeLooseObject(ctx context.Context, store writableGitRemoteStore, typ string, data []byte) error {
	hash := objectHash(typ, data)
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "%s %d", typ, len(data))
	raw.WriteByte(0)
	raw.Write(data)
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return store.write(ctx, "objects/"+hash[:2]+"/"+hash[2:], compressed.Bytes())
}

func objectHash(typ string, data []byte) string {
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "%s %d", typ, len(data))
	raw.WriteByte(0)
	raw.Write(data)
	sum := sha1.Sum(raw.Bytes())
	return hex.EncodeToString(sum[:])
}

func readUint32(data []byte) uint32 {
	return uint32(data[0])<<24 | uint32(data[1])<<16 | uint32(data[2])<<8 | uint32(data[3])
}
