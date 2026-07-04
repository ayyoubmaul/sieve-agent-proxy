# Contributing to Sieve

We love contributions! Here's how to get started.

## Development Setup

```bash
# Clone the repo
git clone https://github.com/ayyoubmaul/sieve-agent-proxy
cd sieve-agent-proxy

# Install dependencies (Go 1.21+)
go mod download

# Build
make build

# Run tests
make test
```

## Project Structure

```
.
├── cmd/
│   ├── sieve/          # Main proxy binary
│   └── bench/          # Benchmark tool
├── docs/               # Documentation
├── .github/workflows/  # CI/CD pipelines
├── public/             # Embedded assets (dashboard)
├── *.go                # Core implementation
├── Makefile            # Build automation
└── README.md
```

## Making Changes

1. **Fork and branch**: Create a feature branch from `main`
   ```bash
   git checkout -b feature/your-feature
   ```

2. **Code style**: Run formatter and linter
   ```bash
   make fmt
   make lint
   ```

3. **Test**: Ensure all tests pass
   ```bash
   make test
   ```

4. **Commit**: Write clear, descriptive commits
   ```bash
   git commit -m "feat: add new feature" -m "Detailed description of changes"
   ```

5. **Push and PR**: Push to your fork and open a Pull Request

## Running Locally

Start the proxy in development mode:

```bash
make run
```

Or with custom config:

```bash
PORT=4142 COMPRESSION=true TOKEN_CACHE=true make run
```

## Testing

Run all tests:
```bash
make test
```

Run tests with coverage:
```bash
make test-coverage
```

## Code Guidelines

- Follow Go idioms ([Effective Go](https://golang.org/doc/effective_go))
- Write table-driven tests for complex logic
- Add comments for non-obvious behavior
- Keep functions small and focused
- Use meaningful variable names

## Commit Message Format

```
<type>: <subject>

<body>

<footer>
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `chore`

Example:
```
feat: add semantic cache support

Implement TF-IDF based semantic caching to catch near-duplicate
queries and reduce API calls.

Closes #42
```

## Questions?

- Open an [issue](https://github.com/ayyoubmaul/sieve-agent-proxy/issues) with the question tag
- Join discussions on pull requests
- Check [USAGE.md](docs/USAGE.md) for feature docs

Thanks for contributing! 🎉
