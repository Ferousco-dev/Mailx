# Security Policy

Security is important to **MailX**, especially because the project is being built to handle email delivery, authentication, API keys, SMTP credentials, domains, and other sensitive infrastructure.

MailX is currently under active development.

## Supported Versions

MailX has not yet reached its first stable release.

| Version | Supported |
| --- | --- |
| Latest development version | ✅ |
| Older development versions | ❌ |
| Stable release | Not yet available |

Security fixes will currently be applied to the latest version of the project.

As MailX begins publishing versioned releases, this section will be updated with an official support policy.

## Reporting a Vulnerability

If you discover a security vulnerability in MailX, **please do not open a public GitHub issue containing vulnerability details.**

Public disclosure before a fix is available could put users or infrastructure at risk.

Instead, report the vulnerability privately through **GitHub Private Vulnerability Reporting** for this repository.

When submitting a report, please include as much information as possible:

- A description of the vulnerability
- The affected component
- Steps to reproduce the issue
- Potential security impact
- Relevant logs or screenshots
- A proof of concept, if appropriate
- A suggested fix, if you have one

Do not include real passwords, API keys, SMTP credentials, access tokens, or other private information in your report.

## What Happens After Reporting?

After receiving a vulnerability report, the issue will be reviewed and validated.

If the vulnerability is confirmed, the goal will be to:

1. Understand the scope and impact.
2. Develop and test a fix.
3. Release the security fix.
4. Provide appropriate credit to the reporter, if requested.
5. Publicly disclose the vulnerability when it is safe to do so.

If the report is determined not to be a security vulnerability, an explanation may be provided to the reporter.

## Security Best Practices

When working with MailX:

- Never commit API keys or passwords.
- Never commit SMTP credentials.
- Keep secrets in environment variables.
- Use strong authentication credentials.
- Rotate credentials if they are accidentally exposed.
- Validate and sanitize external input.
- Keep dependencies updated.
- Use HTTPS/TLS in production environments.
- Apply appropriate rate limiting to public endpoints.
- Follow the principle of least privilege.

Example environment files containing real credentials should **never** be committed to the repository.

Use files such as:

```text
.env.example
```

to document required environment variables without exposing their actual values.

## Responsible Disclosure

Please allow reasonable time for a vulnerability to be investigated and fixed before publicly disclosing it.

Responsible security research and reports that help improve MailX are appreciated.

---

Thank you for helping keep **MailX** secure.# Security Policy

Security is important to **MailX**, especially because the project is being built to handle email delivery, authentication, API keys, SMTP credentials, domains, and other sensitive infrastructure.

MailX is currently under active development.

## Supported Versions

MailX has not yet reached its first stable release.

| Version | Supported |
| --- | --- |
| Latest development version | ✅ |
| Older development versions | ❌ |
| Stable release | Not yet available |

Security fixes will currently be applied to the latest version of the project.

As MailX begins publishing versioned releases, this section will be updated with an official support policy.

## Reporting a Vulnerability

If you discover a security vulnerability in MailX, **please do not open a public GitHub issue containing vulnerability details.**

Public disclosure before a fix is available could put users or infrastructure at risk.

Instead, report the vulnerability privately through **GitHub Private Vulnerability Reporting** for this repository.

When submitting a report, please include as much information as possible:

- A description of the vulnerability
- The affected component
- Steps to reproduce the issue
- Potential security impact
- Relevant logs or screenshots
- A proof of concept, if appropriate
- A suggested fix, if you have one

Do not include real passwords, API keys, SMTP credentials, access tokens, or other private information in your report.

## What Happens After Reporting?

After receiving a vulnerability report, the issue will be reviewed and validated.

If the vulnerability is confirmed, the goal will be to:

1. Understand the scope and impact.
2. Develop and test a fix.
3. Release the security fix.
4. Provide appropriate credit to the reporter, if requested.
5. Publicly disclose the vulnerability when it is safe to do so.

If the report is determined not to be a security vulnerability, an explanation may be provided to the reporter.

## Security Best Practices

When working with MailX:

- Never commit API keys or passwords.
- Never commit SMTP credentials.
- Keep secrets in environment variables.
- Use strong authentication credentials.
- Rotate credentials if they are accidentally exposed.
- Validate and sanitize external input.
- Keep dependencies updated.
- Use HTTPS/TLS in production environments.
- Apply appropriate rate limiting to public endpoints.
- Follow the principle of least privilege.

Example environment files containing real credentials should **never** be committed to the repository.

Use files such as:

```text
.env.example
```

to document required environment variables without exposing their actual values.

## Responsible Disclosure

Please allow reasonable time for a vulnerability to be investigated and fixed before publicly disclosing it.

Responsible security research and reports that help improve MailX are appreciated.

---

Thank you for helping keep **MailX** secure.
