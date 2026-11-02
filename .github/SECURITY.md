# 🔐 Security Policy

Security is a priority. We maintain a proactive stance to identify and fix vulnerabilities in **mixtape**.

<br/>

## 🛠️ Supported & Maintained Versions
Any released version of **mixtape** that is not marked as deprecated is actively supported and maintained.

<br/>

## 📨 Reporting a Vulnerability

If you’ve found a security issue, **please don’t open a public issue or PR**.

Instead, send a private email to:
📧 [mixtape@mrz1818.com](mailto:mixtape@mrz1818.com)

Include the following:

* 🕵️ Description of the issue and its impact
* 🧪 Steps to reproduce or a working PoC
* 🔧 Any known workarounds or mitigations

We welcome responsible disclosures from researchers, vendors, users, and curious tinkerers alike.

<br/>

## 📅 What to Expect

* 🧾 **Acknowledgment** within 72 hours
* 📢 **Status updates** every 5 business days
* ✅ **Resolution target** of 30 days (for confirmed vulnerabilities)

Prefer encrypted comms? Let us know in your initial email—we’ll reply with our PGP public key.
All official security responses are signed with it.

<br/>

## 🧪 Security Tooling

We regularly scan for known vulnerabilities using:

* [`govulncheck`](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck): Checks Go code and dependencies for known vulnerabilities using the Go vulnerability database.
* [`osv-scanner`](https://github.com/google/osv-scanner): As part of our CI security scans, checks Go dependencies against the [OSV](https://osv.dev) vulnerability database. This helps us catch issues in third-party packages early.
* [`gitleaks`](https://github.com/gitleaks/gitleaks): Scans the repository for sensitive data or secrets that may have been accidentally committed to the codebase.

Want to run these yourself?

```sh
magex deps:audit
```

This will check your local build for known issues in Go modules and dependencies.

<br/>

## 🛡️ Security Standards

We follow the [OpenSSF](https://openssf.org) best practices to ensure this repository remains compliant with industry‑standard open source security guidelines.

<br/>

## 🛠️ GitHub Security Workflows

Every push and pull request runs the security scans in [GoFortress](workflows/fortress-security-scans.yml):

- **gitleaks** looks for secrets committed by mistake.
- **govulncheck** checks the code paths Mixtape actually calls against the Go vulnerability database.
- **OSV-Scanner** checks every dependency against the OSV database.

[Dependabot](dependabot.yml) proposes dependency and GitHub Actions updates. Every third-party action is pinned to a commit SHA. For more details, see the workflow files in the [`.github/workflows/`](workflows) directory.
