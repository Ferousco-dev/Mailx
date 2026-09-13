# Contributing to MailX

Thanks for your interest in contributing to **MailX**.

MailX is a mail service being built from scratch using Go. The project is currently in active development, and contributions, ideas, bug reports, and discussions are welcome.

## Getting Started

Fork the repository and clone your fork:

```bash
git clone https://github.com/ferousco-dev/mailx.git
cd mailx
```

Create a new branch for your changes:

```bash
git checkout -b feature/your-feature-name
```

Install/download the Go dependencies:

```bash
go mod download
```

Run the project:

```bash
go run .
```

The exact development commands may change as MailX grows.

## Making Changes

When contributing:

- Keep changes focused and easy to review.
- Follow existing project structure and conventions.
- Write clear and idiomatic Go.
- Avoid unnecessary dependencies.
- Handle errors properly.
- Add comments only where they improve understanding.
- Update documentation when your change affects behavior.
- Add or update tests when appropriate.

Before submitting your changes, format your Go code:

```bash
go fmt ./...
```

Run the tests:

```bash
go test ./...
```

If available, run:

```bash
go vet ./...
```

## Commit Messages

Use short and descriptive commit messages.

Examples:

```text
feat: add email queue
fix: handle failed SMTP connection
docs: update API documentation
test: add mail delivery tests
refactor: simplify message validation
```

## Pull Requests

Before opening a pull request:

1. Make sure your branch is up to date.
2. Make sure the project builds successfully.
3. Run the tests.
4. Describe what you changed and why.
5. Reference any related issue.

Keep pull requests focused on one feature, fix, or improvement whenever possible.

## Reporting Bugs

When reporting a bug, include:

- What happened
- What you expected to happen
- Steps to reproduce the issue
- Relevant logs or error messages
- Your Go version and operating system, when relevant

Please avoid including passwords, API keys, SMTP credentials, tokens, or other sensitive information in issues.

## Feature Requests

Feature ideas are welcome.

When proposing a feature, explain:

- The problem it solves
- How you think it should work
- Why it would be useful to MailX

For large changes, consider opening an issue for discussion before implementing them.

## Security

If you discover a security vulnerability, please avoid publishing sensitive exploit details in a public issue.

MailX will eventually handle security-sensitive areas such as authentication, API keys, email delivery, domains, and SMTP credentials, so security-related contributions should be treated carefully.

## Project Direction

MailX is both a real software project and a learning project.

The goal is to gradually understand and implement the infrastructure behind a modern mail delivery service using Go.

The architecture may change significantly as the project evolves.

## Code of Conduct

Be respectful and constructive when participating in discussions, issues, and pull requests.

Different experience levels are welcome.

## License

By contributing to MailX, you agree that your contributions will be distributed under the license used by this repository.

---

Thanks for helping build **MailX**.
