<p align="left">
  <img src="web/static/brand/pourops.svg" width="56" height="56" alt="PourOps Logo">
</p>

# PourOps

**PourOps** 是面向精酿酒吧与小酒馆的开源单店经营系统：后端基于 Go（`module pourops`）+ PostgreSQL 模块化单体构建，前台内置零构建的后台管理端、POS 点餐收银端与电视酒单数字大屏，单二进制直接内嵌全部页面模板、静态资源与数据库迁移脚本。

---

## 核心能力

- **酒单 · 桶装与盈亏测算（`/admin/beers`、`/admin/kegs`）**：进货入库、按号位开桶上架、下架不挪位自动标为售罄；支持按桶进价、容量与可配置**默认可售比例**（默认 `80%`，预留打酒损耗）及**目标毛利率**（默认 `50%`）自动计算回本线与单杯推荐价，并在入库/编辑/单桶页提供实时交互式试算滑块。
- **历史酒单与多源检索（`/admin/beers/catalog`）**：独立沉淀酒款档案（厂牌 + 酒名唯一去重），支持 CSV / JSON 批量导入导出（`catalog.Codec`），并集成多源酒款搜索聚合接口，单源超时或失败时自动降级。
- **POS 点餐与拆单结账（`/pos/`）**：支持吧台/桌位管理、多杯型（`150 / 330 / 500ml`）子行独立增减与勾选拆单；结账在同一数据库事务内串行锁单并落库 `keg_pours` / `retail_sales` / `payments`，库存不足整单回滚。
- **经营概览与只读审计流水（`/admin/`、`/admin/records`）**：营收/毛利趋势、100% 容量摊销口径核查、POS 分时段实收、零售低库存预警，以及基于 PostgreSQL `RepeatableRead + ReadOnly` 快照的销售订单与营运流水审计台账。
- **账号权限与两步验证（`/admin/users`、`/account/security`）**：BOSS / 店长 / 店员三角色隔离；支持基于 TOTP 的两步验证（AES-GCM 密钥加密 + 一次性恢复码 + 连续试错锁定 + `auth_version` 全端会话失效）。
- **电视酒单大屏（`/screen/`）**：适配 1920×1080 与竖屏的固定网格大屏，通过 `/api/screen/events` SSE 毫秒级同步上下架与售罄状态，断连自动退回 60s 轮询与内置 `playlist.json` 兜底展示。
- **门店设置与异步 Webhook（`/admin/settings`）**：BOSS 可在线调整门店品牌名、默认可售比例/目标毛利率、Webhook 地址/密钥与订阅事件，在数据库事务 `Commit` 成功后异步发送带 HMAC-SHA256 `X-Bar-Signature` 签名的事件通知。

---

## 架构与目录结构

项目遵循标准 Go 工程布局，采用**薄入口 + 严格单向依赖 DAG 分包**的模块化单体设计：

```text
├── cmd/pourops/
│   └── main.go                     # CLI 入口：读取环境变量、连接 Postgres、初始化迁移并启停服务
├── internal/
│   ├── app/                        # 应用组合根：静态资源清单、模板渲染器、8 个领域服务装配与 HTTP 路由总装
│   ├── platform/                   # 基础设施：PostgreSQL 迁移锁、测试隔离库、表单校验、营运台账、CSRF 中间件
│   ├── event/                      # 进程内异步事件总线（EventBus）与 HMAC-SHA256 Webhook 分发器
│   ├── ui/                         # 布局 ViewModel（Layout/Page）、角色守卫、{{asset}} 指纹与模板渲染器
│   ├── settings/                   # 门店系统参数缓存（SettingsStore）与 BOSS 设置页面
│   ├── auth/                       # 会话签名、权限守卫、账号管理、TOTP 2FA 与恢复码
│   ├── catalog/                    # 酒款主档、历史酒单维护与 Codec 导入导出注册表
│   ├── lookup/                     # 多源酒款检索聚合（Provider 接口与本地/远端检索实现）
│   ├── screen/                     # 电视大屏接口（/screen/、/screen/playlist.json、/api/screen/playlist、SSE Hub）
│   ├── inventory/                  # 酒桶进销存、开/结桶状态机、可配置盈亏测算与零售 SKU 管理
│   ├── pos/                        # POS 台位、加减单品、拆单结账与 PaymentGateway / ReceiptPrinter 扩展点
│   └── report/                     # 经营概览、销售趋势、POS 分时段实收与只读审计流水
├── migrations/                     # 0001–0011 PostgreSQL 结构迁移（go:embed，启动自动执行）
└── web/
    ├── embed.go                    # 统一导出 TemplatesFS 与 StaticFS
    ├── templates/                  # Go html/template 页面（layouts/、admin/、pos/、screen/）
    ├── static/                     # 静态资源（brand/、css/、js/、screen/playlist.json、vendor/）
    └── tests/                      # 零依赖 Node 前端回归脚本（screen_test.mjs、keg_pricing_test.mjs）
```

### 编译期扩展点与事件机制

- **`catalog.Codec` / `CodecRegistry`**：注册新的历史酒单导入导出格式（默认内置 CSV / JSON）。
- **`lookup.Provider` / `Service`**：扩展酒款检索数据源，支持按序聚合与单源故障降级。
- **`pos.PaymentGateway` & `pos.ReceiptPrinter`**：替换或扩展 POS 收款渠道确认与热敏小票打印（默认 `ManualGateway` / `NoopPrinter`）。
- **`event.EventBus` & `WebhookDispatcher`**：支持 `screen.changed`、`beer.purchased`、`beer.tapped`、`keg.finished`、`order.paid`、`retail.low_stock`、`webhook.test` 领域事件（均在数据库事务 `Commit` 成功后发布）。

---

## 快速部署

### 1. 环境准备与本地快速启动

前置依赖：**Go 1.24+** 与 **PostgreSQL**（推荐 14–17）。

```bash
# 获取源码并构建单二进制文件
git clone https://github.com/L14nY1Wang/PourOps.git
cd PourOps
go build -o pourops ./cmd/pourops

# 创建空数据库（名称需与 DATABASE_URL 一致）
createdb bar

# 首次启动（users 表为空时必须提供 ≥6 位的 BOSS_PASSWORD）
BOSS_PASSWORD='请更换为至少六位密码' \
DATABASE_URL='postgres://localhost:5432/bar?sslmode=disable' \
ADDR='127.0.0.1:8080' \
./pourops
```

启动完成后即可访问：

- 运营后台登录：`http://127.0.0.1:8080/admin/login`
- 吧台 POS 点餐：`http://127.0.0.1:8080/pos/`
- 电视酒单大屏：`http://127.0.0.1:8080/screen/`

### 2. 环境变量说明

所有环境变量均支持按需覆盖，空串按未设置处理：

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DATABASE_URL` | `postgres://localhost:5432/bar?sslmode=disable` | PostgreSQL 连接串 |
| `ADDR` | `:8080` | HTTP 监听地址 |
| `BRAND` | `PourOps` | 默认门店名称（首次后可在 `/admin/settings` 持久化修改） |
| `BOSS_PASSWORD` | 无 | 仅在 `users` 表为空时初始化 `boss` 账号，要求 ≥6 位，不写入日志 |
| `SESSION_SECRET` | 进程随机 | 会话与 2FA 挑战签名密钥；不设则每次重启后现有会话失效 |
| `TWO_FACTOR_KEY` | 无 | 32 字节十六进制密钥（`openssl rand -hex 32`），用于加密 TOTP 密钥；缺失时不可新启用 2FA |
| `JIUHE_API_BASE` | `https://mall.jiuhehe.com/api` | 远端酒款检索 API 地址（置空可禁用远端检索） |
| `TEST_DATABASE_URL` | 无 | 集成测试专用数据库 DSN；未设时自动跳过数据库集成用例 |

### 3. 单二进制生产部署

`./pourops` 已通过 `go:embed` 将模板、静态资源、字体图标与数据库迁移脚本全部打包进单个可执行文件，**不依赖仓库源码或额外目录**：

1. 将编译产物 `./pourops` 复制到目标主机任意目录（如 `/opt/pourops/pourops`），支持跨平台交叉编译（如 `GOOS=linux GOARCH=amd64 go build -o pourops ./cmd/pourops`）。
2. 在目标主机准备独立的环境变量文件（务必固定 `SESSION_SECRET` 与 `TWO_FACTOR_KEY`，首次初始化完成后可移除 `BOSS_PASSWORD`），由 `systemd` 或进程守护器托管运行（默认监听 `127.0.0.1:8080` 供反向代理接入）。
3. 建议使用 Nginx / Caddy 等反向代理提供 HTTPS 与 `/api/screen/events` SSE 长连接转发（注意关闭 SSE 路径响应缓冲）。
4. 升级时只需替换新二进制并重启进程；服务启动时会自动获取 PostgreSQL advisory lock 并按序执行尚未运行的数据库迁移。

---

## 测试与回归验证

```bash
# Go 纯单元测试（无 TEST_DATABASE_URL 时自动跳过数据库集成用例）
go test ./...

# Go 全量集成与并发测试（请使用独立测试库，每个测试会自动创建并清理隔离 schema）
createdb bar_review_test
TEST_DATABASE_URL='postgres://localhost:5432/bar_review_test?sslmode=disable' go test -race -count=1 ./...

# 桶装测算滑块与电视大屏 JS 回归（零依赖原生 Node）
node web/tests/keg_pricing_test.mjs
node web/tests/screen_test.mjs
```
