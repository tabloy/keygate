<div align="center">

<img src="web/public/logo.svg" width="72" height="72" alt="Keygate" />

# Keygate

**Open source software license management platform.**

The self-hosted alternative to Keygen, Cryptlex, and LicenseSpring.

[Website](https://keygate.app) · [Documentation](https://keygate.app/docs) · [Community](https://github.com/tabloy/keygate/discussions)

[![License](https://img.shields.io/badge/license-AGPL%20v3-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/tabloy/keygate?label=release&color=green)](https://github.com/tabloy/keygate/releases)
[![Stars](https://img.shields.io/github/stars/tabloy/keygate?style=flat)](https://github.com/tabloy/keygate/stargazers)
[![Sponsor](https://img.shields.io/badge/sponsor-❤-ff69b4)](https://keygate.app/sponsorships)

**[English](README.md)** · **[简体中文](README.zh-CN.md)**

<br />

<img src="web/public/screenshot.png" width="800" alt="Keygate Dashboard" />

</div>

<br />

## Why Keygate?

You've built great software. Now you need to decide who can use it, how they pay for it, and what features they get access to.

Commercial license platforms charge per-seat, per-month, and your customer data lives on someone else's servers. Building your own takes months of engineering on activation logic, payment webhooks, quota tracking, and all the edge cases that come at 2 AM.

**Keygate is the middle ground.** A production-ready license server you deploy on your own infrastructure, connect to your own Stripe or PayPal, and manage through a clean dashboard. It handles everything from activation to dunning — so you can focus on building your product.

One binary. One database. Full control. Free, forever.

<br />

## Who is it for?

| | |
|:---|:---|
| **🧑‍💻 Indie Developers** — Selling a desktop app, CLI tool, or Electron app? Keygate handles license keys, activation limits, and trials so you can focus on shipping. | **🏢 SaaS Companies** — Managing subscription tiers with different feature sets? Define plans with entitlements, track usage, and let Stripe handle billing automatically. |
| **🏭 Enterprise Vendors** — Need floating licenses for large teams? Concurrent seat checkout with heartbeat monitoring, perfect for shared-seat environments. | **⚡ API Providers** — Enforcing rate limits and usage quotas? Atomic quota enforcement tracks every call and warns customers before they hit limits. |

<br />

## Features

### 🔑 License Management

Every model in one platform — **subscriptions**, **perpetual**, **trials**, and **floating** (concurrent) licenses. Create, activate, verify, suspend, reinstate, and revoke with full audit trail. Per-device or per-user activation limits. Grace periods. License keys hashed with SHA-256. Signed tokens for offline verification.

### 📊 Usage Metering

Track API calls, storage, bandwidth, or any custom metric. Quotas enforced **atomically at the database level** — even under high concurrency, limits are never exceeded. Hourly, daily, monthly, or yearly cycles with automatic reset. Threshold warnings via webhooks.

### 💳 Payments

Stripe and PayPal integrated end-to-end. Customer pays → license created automatically. Payment fails → dunning emails on schedule. Supports checkout, plan upgrades/downgrades with proration, cancellations, refunds, and billing portal.

### 👥 Team Seats & Entitlements

Customers manage their own teams within a license. Seat roles (owner/admin/member), configurable limits per plan. Feature entitlements as boolean flags, numeric limits, or usage quotas. Purchasable add-ons that extend plan capabilities.

### 📈 Admin Dashboard

Products, plans, licenses, customers, API keys, webhooks, analytics, audit logs, team management, email templates, and brand customization — all from one interface. Search, filter, and export (CSV/JSON).

### 🛡️ Security

OAuth2 login (GitHub/Google), role-based access checked per-request from database, brute-force protection, rate limiting, HMAC-signed webhooks, SameSite cookies, HSTS, and startup validation that rejects weak secrets.

### 🌍 Self-Hosted

Single Go binary + PostgreSQL. No Redis, no microservices. Auto-migration on startup. Setup wizard for first run. Custom branding, email templates, and i18n (English/Chinese built-in).

<br />

## Quick Start

### Docker (recommended)

```bash
# 1. Download
curl -O https://raw.githubusercontent.com/tabloy/keygate/main/docker-compose.yml
curl -O https://raw.githubusercontent.com/tabloy/keygate/main/.env.example
cp .env.example .env

# 2. Set your secrets
# Edit .env: set JWT_SECRET and LICENSE_SIGNING_KEY (openssl rand -hex 32)

# 3. Run
docker compose up -d
```

### From source

```bash
git clone https://github.com/tabloy/keygate.git
cd keygate && cp .env.example .env
make build && ./bin/keygate
```

Open **http://localhost:9000** — the setup wizard guides you from there.

> 📖 Full docs, deployment guides, and SDK examples at **[keygate.app/docs](https://keygate.app/docs)**

<br />

## Compared to Alternatives

| | **Keygate** | Keygen | Cryptlex | LicenseSpring |
|:---|:---:|:---:|:---:|:---:|
| Open source | **✅ AGPL v3** | Partial | ❌ | ❌ |
| Self-hosted | **✅** | ✅ | ❌ | ❌ |
| Price | **Free** | From $99/mo | From $249/mo | From $50/mo |
| Floating licenses | ✅ | ✅ | ✅ | ✅ |
| Usage metering | **✅** | ❌ | ❌ | ❌ |
| Built-in payments | **✅** | ❌ | ❌ | ❌ |
| Customer portal | ✅ | ❌ | ✅ | ✅ |
| Admin dashboard | ✅ | ✅ | ✅ | ✅ |
| Webhook system | ✅ | ✅ | ✅ | ✅ |
| Audit trail | ✅ | ✅ | ❌ | ❌ |
| i18n | ✅ | ❌ | ❌ | ❌ |

<br />

## Community

- **[Discussions](https://github.com/tabloy/keygate/discussions)** — Questions, ideas, show & tell
- **[Issues](https://github.com/tabloy/keygate/issues)** — Bug reports and feature requests
- **[Blog](https://keygate.app/blog)** — Updates and engineering stories
- **[Sponsor](https://keygate.app/sponsorships)** — Support the project

## Contributing

All contributions welcome — bugs, features, docs, translations. Check [open issues](https://github.com/tabloy/keygate/issues) or start a [discussion](https://github.com/tabloy/keygate/discussions), then submit a PR.

## License

[AGPL v3 License](LICENSE) with additional terms per [Section 7(b)](https://www.gnu.org/licenses/agpl-3.0.en.html#section7) — Copyright © 2026 [Tabloy](https://tabloy.app)

You are free to fork, modify, and self-host this software under the AGPL v3. The **"Powered by Keygate"** attribution in the UI must be preserved (see [NOTICE](NOTICE)). A commercial license to remove the attribution is available — contact [hello@keygate.app](mailto:hello@keygate.app).

## Star History

<a href="https://star-history.com/#tabloy/keygate&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=tabloy/keygate&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=tabloy/keygate&type=Date" />
   <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=tabloy/keygate&type=Date" width="600" />
 </picture>
</a>

---

<div align="center">
<sub>If Keygate helps your business, consider giving it a ⭐</sub>
</div>
