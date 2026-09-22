<div align="center">

<img src="web/public/logo.svg" width="72" height="72" alt="Keygate" />

# Keygate

**开源软件许可证管理平台。**

可自托管的 Keygen、Cryptlex、LicenseSpring 替代方案。

[官网](https://keygate.app) · [文档](https://keygate.app/docs) · [社区](https://github.com/tabloy/keygate/discussions)

[![License](https://img.shields.io/badge/license-AGPL%20v3-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/tabloy/keygate?label=release&color=green)](https://github.com/tabloy/keygate/releases)
[![Stars](https://img.shields.io/github/stars/tabloy/keygate?style=flat)](https://github.com/tabloy/keygate/stargazers)
[![Sponsor](https://img.shields.io/badge/sponsor-❤-ff69b4)](https://ko-fi.com/alanisme)

**[English](README.md)** · **[简体中文](README.zh-CN.md)**

<br />

<img src="web/public/screenshot.png" width="800" alt="Keygate 管理后台" />

</div>

<br />

## 为什么选择 Keygate？

你做了一款出色的软件。现在需要决定谁能用它、怎么收费、以及开放哪些功能。

商业许可证平台按席位、按月收费，而且你的客户数据存储在别人的服务器上。自己开发一套需要数月的工程投入 — 激活逻辑、支付回调、配额追踪，还有凌晨两点出现的各种边界情况。

**Keygate 是两者之间的最优解。** 一个生产就绪的许可证服务器，部署在你自己的基础设施上，接入你自己的 Stripe，通过简洁的管理后台统一管理。从激活到催款，全部搞定 — 让你专注于构建产品。

一个二进制文件。一个数据库。完全掌控。永久免费。

<br />

## 适合谁？

| | |
|:---|:---|
| **🧑‍💻 独立开发者** — 在卖桌面应用、CLI 工具或 Electron 应用？Keygate 帮你管理许可证密钥、激活限制和试用期，让你专注发布新功能。 | **🏢 SaaS 公司** — 需要管理不同功能集的订阅层级？定义计划和权限，追踪用量，让 Stripe 自动处理计费。 |
| **🏭 企业软件厂商** — 需要为大团队提供浮动许可？并发席位签出配合心跳监控，完美适配共享席位场景。 | **⚡ API 服务商** — 需要执行速率限制和用量配额？原子级配额执行追踪每一次调用，在客户达到限额前发出预警。 |

<br />

## 功能特性

### 🔑 许可证管理

一个平台覆盖所有模式 — **订阅制**、**永久买断**、**买断加维护期**（一次付清，含一年更新，门户内续费）、**免费试用**和**浮动许可**（并发）。创建、激活、验证、暂停、恢复和吊销，全程审计追踪。按设备或按用户的激活限制，**原子级强制**（重试不会重复计数）。宽限期。许可证密钥 SHA-256 哈希存储，落盘加密。Ed25519 签名 token 支持离线验证（服务端持私钥,客户端通过 `/license/pubkey` 拿公钥本地验签）。**`Idempotency-Key` 头**支持 — 写入重试永不重复。

公开 SDK 端点（activate / verify / deactivate / usage / download）直接用 `license_key`，无需在二进制里嵌入 API 密钥。客户可在门户自助释放激活槽（丢笔记本不用提工单）。

### 🚀 软件分发

向已安装客户端推送签名更新。**Sparkle**（macOS）、**Velopack**（Windows）和 **Tauri**（跨平台）的自动更新器消费同一份 release 数据 — 一次发布，所有更新器兼容。每个 release 下挂多平台二进制，**原子发布门控**（绝不泄露半上传状态），**yank** 即时回滚。每产品 **Ed25519 签名密钥**，私钥用 AES-256-GCM + HKDF 派生子密钥落盘加密。服务端算 SHA-256（不信客户端哈希）。stable channel 公开访问 — 客户的自动更新器在 license 轮换时绝不中断。按维护期销售的产品打开 `feed_license_required`，更新器随请求带上许可证，feed 只列出客户更新期内发布的版本；软件本身照常运行。每产品 `minimum_supported_version` 强制升级下限。

对象存储兼容 S3 — Cloudflare R2、AWS S3、MinIO 等任何说 SigV4 的存储。Presigned URL 浏览器直传（不经 Keygate 中转），license 校验后短期下载 URL。

### 📊 用量计量

追踪 API 调用次数、存储空间、带宽或任何自定义指标。配额在**数据库层原子执行** — 即使在高并发下也不会超限。支持小时、天、月、年周期，自动重置。通过 Webhook 发送阈值预警。

### 💳 支付集成

Stripe 端到端集成，**三层可靠性保障** — Webhook、成功页验证和定时扫描确保每笔付款都不遗漏。客户付款 → 许可证自动创建。付款失败 → 按计划发送催款邮件。支持结账、升降级（按比例计费）、取消、退款、账单管理门户，以及客户在门户内一次性续费永久许可证的更新期。Stripe Webhook 自动配置 — 只需设置 API 密钥。

### 👥 团队席位与权限

客户在许可证内管理自己的团队。席位角色（所有者/管理员/成员），每个计划可配置上限。功能权限支持布尔标志、数值限制或用量配额。可购买的附加组件扩展计划能力。

### 🔧 服务端到服务端 API

通过 `Authorization: Bearer kg_live_…` 程序化访问 admin — Stripe webhook 触发后自动建 license、cron 跑用量导出、`/admin/*` 能做的所有事都能自动化。**基于 scope 的鉴权**，默认 fail-closed（空 scope 的 key 什么都不能做）。系统级 admin key 或产品级 key，同 Stripe `sk_live_` 和 GitHub PAT 一类。

### 📈 管理后台

产品、计划、许可证、客户、API 密钥、Webhook、分析、审计日志、团队管理、邮件模板和品牌自定义 — 全部在一个界面完成。搜索、筛选和导出（CSV/JSON）。

### 🛡️ 安全

邮箱 OTP 登录（常量时间哈希验证），基于数据库的每次请求角色权限检查，暴力破解防护，速率限制，HMAC 签名 Webhook，SameSite Cookie，HSTS，启动时拒绝弱密钥。License verify 端点将所有"license-knowable"失败统一为 404，关闭 `license_key` 枚举 oracle。Idempotency-Key 中间件防止重试导致的重复执行。

### 🌍 自托管

单个 Go 二进制文件 + PostgreSQL +（可选）S3 兼容存储用于 release 二进制。无需 Redis，无需微服务。启动时自动迁移。首次运行有安装向导。支持自定义品牌、邮件模板和国际化（内置中英文）。

<br />

## 快速开始

### Docker（推荐）

```bash
# 1. 下载
curl -O https://raw.githubusercontent.com/tabloy/keygate/main/docker-compose.yml
curl -O https://raw.githubusercontent.com/tabloy/keygate/main/.env.example
cp .env.example .env

# 2. 设置密钥
# 编辑 .env：设置 JWT_SECRET 和 LICENSE_SIGNING_KEY（openssl rand -hex 32）

# 3. 启动
docker compose up -d
```

### 从源码构建

```bash
git clone https://github.com/tabloy/keygate.git
cd keygate && cp .env.example .env
make build && ./bin/keygate
```

打开 **http://localhost:9000** — 安装向导会引导你完成初始配置。

> 📖 完整文档、部署指南和 SDK 示例请访问 **[keygate.app/docs](https://keygate.app/docs)**

<br />

## 与竞品对比

| | **Keygate** | Keygen | Cryptlex | LicenseSpring |
|:---|:---:|:---:|:---:|:---:|
| 开源 | **✅ AGPL v3** | 部分 | ❌ | ❌ |
| 可自托管 | **✅** | ✅ | ❌ | ❌ |
| 价格 | **免费** | $99/月起 | $249/月起 | $50/月起 |
| 浮动许可 | ✅ | ✅ | ✅ | ✅ |
| 用量计量 | **✅** | ❌ | ❌ | ❌ |
| 内置支付 | **✅** | ❌ | ❌ | ❌ |
| 自动更新分发 | **✅ Sparkle / Velopack / Tauri** | ✅ 付费模块 | ❌ | ❌ |
| 客户门户 | ✅ | ❌ | ✅ | ✅ |
| 管理后台 | ✅ | ✅ | ✅ | ✅ |
| Webhook | ✅ | ✅ | ✅ | ✅ |
| 审计日志 | ✅ | ✅ | ❌ | ❌ |
| Idempotency-Key | **✅** | ❌ | ❌ | ❌ |
| 多语言 | ✅ | ❌ | ❌ | ❌ |

<br />

## 社区

- **[讨论区](https://github.com/tabloy/keygate/discussions)** — 提问、分享想法
- **[Issues](https://github.com/tabloy/keygate/issues)** — Bug 报告和功能请求
- **[博客](https://keygate.app/blog)** — 产品更新和技术文章
- **[赞助](https://keygate.app/sponsorships)** — 支持项目发展

## 赞助

Keygate 免费且可自托管。如果它帮你省下了托管授权服务的开销，在 [Ko-fi 上赞助](https://ko-fi.com/alanisme) 能让开发持续下去。

<!-- sponsors:start -->
_虚位以待。_
<!-- sponsors:end -->

## 贡献

欢迎所有形式的贡献 — Bug 修复、新功能、文档改进、翻译等。查看 [open issues](https://github.com/tabloy/keygate/issues) 或发起 [讨论](https://github.com/tabloy/keygate/discussions)，然后提交 PR。

## 商业授权

使用 Keygate 是免费的，商用也免费。自己部署运行、用它给你销售的软件发授权、面向自己的客户提供服务，都不用付费。

AGPL 只要求两件事：界面上保留 **"Powered by Keygate"** 署名；如果你修改了 Keygate 并让别人使用，把改动公开。商业授权解除这两条，白标或以闭源服务转售需要它。

请联系 [hello@keygate.app](mailto:hello@keygate.app)。

## 许可证

[AGPL v3 License](LICENSE)（附 [Section 7(b)](https://www.gnu.org/licenses/agpl-3.0.en.html#section7) 附加条款）— Copyright © 2026 [Tabloy](https://tabloy.app)

你可以在 AGPL v3 下自由 fork、修改和自托管本软件。UI 中的 **"Powered by Keygate"** 署名须保留（详见 [NOTICE](NOTICE)）。

## Star 趋势

<a href="https://star-history.com/#tabloy/keygate&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=tabloy/keygate&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=tabloy/keygate&type=Date" />
   <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=tabloy/keygate&type=Date" width="600" />
 </picture>
</a>

---

<div align="center">
<sub>如果 Keygate 对你的业务有帮助，请给我们一个 ⭐</sub>
</div>
