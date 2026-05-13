# Contributing

Contributions are welcome. The usual workflow is fork, branch, commit, and open
a pull request.

## Workflow

1. Fork the repository on GitHub.
2. Clone your fork:

```bash
git clone https://github.com/YOUR-USER/bgit.git
cd bgit
```

3. Add the upstream repository:

```bash
git remote add upstream https://github.com/bucketgit/bgit.git
```

4. Create a feature branch:

```bash
git checkout -b feature/my-change
```

5. Make your changes and add tests where behavior changes.
6. Run the checks:

```bash
go test ./...
go build ./...
```

7. Commit your changes:

```bash
git add .
git commit -m "Describe the change"
```

8. Push your branch:

```bash
git push origin feature/my-change
```

9. Open a pull request against `bucketgit/bgit`.

## Pull Requests

Please keep pull requests focused. Include:

- A short description of the change.
- Any relevant usage examples.
- Notes about tests you ran.

For user-facing changes, update `README.md` or `CHANGELOG.md` when appropriate.
