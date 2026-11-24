# PourOps — Repository Guidelines

**PourOps**：单店酒吧经营系统：Go（`module pourops`）+ Postgres 后台（进销存、桶装盈亏、POS 点餐结账、订单/流水记录、账号与可选 2FA），加一个零依赖的电视酒单数字大屏。三个角色：BOSS（全部）、店长（全部后台、管不了账号）、店员（仅 `/pos/`）。

## 开发与交付约定

- 构建、单元测试、集成测试（隔离测试库）及浏览器验收均在本地工作区完成；提交前确保 `gofmt -l .` 为空且全量测试通过。
- 仓库只保留开源应用源码与内嵌资产，绝不提交真实环境密钥、`.env`、站点域名、私网地址或特定主机部署脚本。

## Architecture & Data Flow

标准 Go 模块化单体布局：CLI 入口 `cmd/pourops/main.go` + 应用组合根 `internal/app/` + 领域分包 `internal/{platform,event,ui,settings,auth,catalog,lookup,screen,inventory,pos,report}`（严格单向依赖 DAG，不使用运行时动态加载插件，扩展能力走编译期接口与领域事件总线）：

```text
platform (zero internal deps)
  ├── event
  ├── ui
  └── settings (depends on platform, event, ui)
        └── domain packages: auth, catalog, lookup, screen, inventory, pos, report
              └── app (composes services, assets, renderer, mux + CrossOriginProtect)
                    └── cmd/pourops/main.go
```

- **启动链路**：`platform.Env` → `pgxpool` → `platform.Migrate`（`pg_advisory_lock(0x6e6f727468706965)` 逐文件事务，读取 `migrations.FS`）→ `auth.SeedBoss`（`users` 为空才需要 `BOSS_PASSWORD`）→ `settings.SettingsStore.Refresh` → `event.NewEventBus()` + `WebhookDispatcher.Attach` + `screen.NewHub(bus)` → `catalog.NewPosterStore(POSTER_DIR)`（+ 后台异步本地化历史远程酒标）→ `app.NewAssets()` + `app.NewRenderer(...)` + `app.NewHandler(...)` → `platform.CrossOriginProtect` → 5s 优雅关停（先关 HTTP 再排空异步事件队列）。
- **前端与迁移单二进制内嵌**：`web/embed.go` 统一导出 `TemplatesFS`（`web/templates/{layouts,admin,pos,screen}`）与 `StaticFS`（`web/static/{brand,css,js,screen,vendor}`），`migrations/embed.go` 导出 `migrations.FS`。修改模板、静态资源或迁移脚本后直接 `go build ./cmd/pourops` 即可单二进制部署，无需保留源码目录。
- **电视大屏平等归口**：电视页与后台/POS 平等作为页面模块，`web/templates/screen/screen.html` 由 `ui.Renderer.RenderShell(w, r, "screen/screen.html", "screen", &ScreenPage{})` 统一渲染（自动注入 `Brand` 与 `{{asset}}` 指纹）；本地兜底数据 `web/static/screen/playlist.json` 由 `screen.Service.PlaylistJSON` 从根 `web.StaticFS` 挂载提供（`/screen/playlist.json`）；Node 测试脚本位于 `web/tests/`（不被 `go:embed` 打进二进制）。
- **编译期接口扩展点**（不引入运行时插件）：
  - `catalog.Codec` + `CodecRegistry`：历史酒单导入导出格式（系统设置中默认使用含 `catalog.json` + 本地酒标图片的 `.zip` 压缩包 `ZIPCodec`，内部保留 CSV / JSON 编解码器）。
  - `lookup.Provider` + `lookup.Service`：酒款自动检索与详情补充（默认按序聚合 `LocalCatalogProvider` 与 `JiuheProvider`，单源超时或失败降级为 warning）。
  - `pos.PaymentGateway`（默认 `ManualGateway`）与 `pos.ReceiptPrinter`（默认 `NoopPrinter`）：POS 结账渠道确认与提交后热敏小票打印扩展点。
- **轻量进程内事件总线与异步 Webhook**：`event.EventBus` + `event.WebhookDispatcher`（HMAC-SHA256 `X-Bar-Signature` 签名、`settings.SettingsStore` 提供 URL/密钥/订阅事件、SSRF 基本校验、8s 超时 + 1 次重试）。领域事件（`screen.changed`、`beer.purchased`、`beer.tapped`、`keg.finished`、`order.paid`、`retail.low_stock`、`webhook.test`）**一律在数据库事务 `Commit` 之后发布**，绝不在未提交事务内发事件或做外网 I/O。
- **销售只在结账时记账**：`order_items` 是暂存，`pos.PayItemsWithGateway`（`internal/pos/store.go`）单事务里锁订单行 → 锁未付明细行 → 落 `keg_pours` / `retail_sales` → 调用 `PaymentGateway.Settle` → 写 `payments` → 标 `paid_at` → 结清则 `closed_at`。概览盈亏口径因此无需回补。
- **行锁顺序固定**：酒款行 → 桶行；订单行 → 明细行。加购/改量/结账共用 `lockOpenOrder` 串行。
- **营运流水与业务同事务**：`platform.LogOperation`（`internal/platform/oplog.go`）写 `operation_events`，业务回滚流水一起没；操作人经 `platform.WithActor(ctx, username)` 透传，守卫层已塞好。
- **会话失效靠 `auth_version`**：cookie 票据 `uid.ver.exp.mac` 绑版本号，改密码/启停 2FA/停用都 `auth_version+1` 一次性踢掉所有旧会话与登录挑战。
- **`ConsumeSecondFactor` 锁约定**（`internal/auth/security.go`）：调用方必须先 `for update` 锁 `users` 行；它不开事务不提交；缺 `TWO_FACTOR_KEY` 或解密失败返回 error **绝不降级成单密码放行**。
- **SSE 广播**：`screen.NewHub(bus)` 订阅 `event.EventScreenChanged` → `/api/screen/events` 推 `changed` → 电视页重拉 `/api/screen/playlist`；慢消费者丢信号，靠 60s 轮询兜底。
- **成本核算口径与系统参数**：经营概览与数据分析（`internal/report/{overview,sales,pos_insights}.go`）统一按酒吧**营业日**（北京时间当日 `07:00` 至次日 `07:00`，`platform.BusinessDayCutoff`）统计，桶装按实际售出毫升占整桶 **100% 容量**摊销单杯成本（`cost_cents * ml / capacity_ml`）；桶装盈亏与定价测算（`internal/inventory/economics.go`）按 `store_settings` 可配置的 **默认可售比例**（默认 80%，预留损耗）与 **目标毛利率**（默认 50%）计算单杯有效成本、回本线与推荐价（`/admin/settings` BOSS 可调，`web/static/js/keg-pricing.js` 通过 `data-default-sellable` / `data-default-margin` 联动）。
- 记录页（`internal/report/records.go`）用 `RepeatableRead + ReadOnly` 单快照读同一批表做审计聚合。

数据链：`beers 1—n kegs 1—n keg_pours`；`beer_catalog` 独立沉淀历史酒款资料（按 `lower(btrim(brewery)), lower(btrim(name))` 唯一去重）；`orders 1—n order_items / payments`；`order_items.kind` 多态指向 beer/retail（无外键）；`retail_sale_items` 故意不设 FK 存名称价格快照；`operation_events` 无外键纯台账；`store_settings` 保存品牌名/默认出酒率与毛利率/Webhook 配置；台位唯一性靠 partial index `orders_open_spot where closed_at is null`。

## Key Directories

|目录|用途|
|---|---|
|`cmd/pourops/`|可执行程序入口（`main.go`），在仓库根目录运行 `go` 命令|
|`internal/app/`|应用组合根（`Deps`、`NewAssets`、`NewRenderer`、`NewHandler`）及全 19 套模板/全路由冒烟测试|
|`internal/platform/`|最底层无内部依赖基建：迁移/隔离测试库、严格表单与格式化、错误处理、操作台账、CSRF 中间件|
|`internal/event/`|进程内轻量事件总线 `EventBus`、领域事件常量与异步 HMAC-SHA256 `WebhookDispatcher`|
|`internal/ui/`|布局 ViewModel（`Layout`、`Page`、`UserView`、`Guards`）、`AssetManifest`（将 `css/*.css` 拼为 `/static/css/admin.bundle.css`，保留 `/static/css/admin.css` 别名）与 `Renderer`|
|`internal/settings/`|门店系统参数 `SettingsStore` 与 BOSS 专属 `/admin/settings`（品牌/出酒率与毛利率/分类管理/历史酒单 ZIP 导入导出/Webhook 配置）|
|`internal/auth/`|登录会话、三种守卫、账号管理、TOTP / AES-GCM / 恢复码两步验证|
|`internal/catalog/`|酒款主档、后台历史酒单档案、`PosterStore` 本地酒标（`/posters/{name}`，唯一文件名、替换/清空回收旧图）与 `.zip` `Codec`|
|`internal/lookup/`|多源酒款搜索聚合 `Provider` 扩展点（本地历史酒单 + 酒核 UT 同步站）|
|`internal/screen/`|电视酒单服务：`/screen/`（渲染 `screen/screen.html`）、`/screen/playlist.json`、`/api/screen/playlist` 与 SSE `Hub`|
|`internal/inventory/`|酒桶进销存、开/结桶状态机、可配置桶装盈亏测算、零售 SKU 与出入库、业务事件发布|
|`internal/pos/`|POS 台位/加减单品/拆单结账、`PaymentGateway` 与 `ReceiptPrinter` 扩展点|
|`internal/report/`|经营概览、销售趋势、POS 分时段实收、销售订单与营运流水只读审计|
|`migrations/`|`0001`–`0012` 启动时自跑，`schema_migrations` 记账（`embed.go` 导出 `migrations.FS`）|
|`web/`|全站前端唯一归口：`embed.go`（导出 `TemplatesFS`、`StaticFS`）、`templates/{layouts,admin,pos,screen}/`、`static/{brand,css,js,screen,vendor}/`、`tests/`（不进 embed）|

## Development Commands

```bash
# 构建并在本机运行（首次 users 为空必须 BOSS_PASSWORD≥6）
go build -o pourops ./cmd/pourops
BOSS_PASSWORD=xxxxxx DATABASE_URL='postgres://localhost:5432/bar?sslmode=disable' ADDR=127.0.0.1:8080 ./pourops

# 纯单元测试（DB 用例在未设 TEST_DATABASE_URL 时自动 skip）
go test ./...

# 全量测试（含 DB 集成 + 并发用例；库必须是独立的，绝不指向生产）
createdb bar_review_test
TEST_DATABASE_URL='postgres://localhost:5432/bar_review_test?sslmode=disable' go test -race -count=1 ./...

# 前端 JS 回归（零依赖原生 Node）
node web/tests/screen_test.mjs
node web/tests/keg_pricing_test.mjs
```

无 Makefile、无 CI、无 lint 配置；格式工具就是 `gofmt`，提交前 `gofmt -l .` 应为空。Go 版本钉在 1.24（`http.NewCrossOriginProtection` 不可用，CSRF 是手写中间件 `platform.CrossOriginProtect`，见 `internal/platform/middleware.go`），**不要随手升 `go` 指令**。

## Code Conventions & Common Patterns

- **金额一律 `int64` 分**（`*_cents`），展示用 `platform.PriceText` / `platform.MoneyText`；杯型 150/330/500 三档价，`Beer.SyncPrice` 回填主价。
- **错误处理**：面向用户的表单错误走 `platform.URLMsg` 中文短语塞进 redirect query，页面 `?err`/`?msg` 显示，可展示校验错误显式包为 `platform.UserErr`；服务器内部错误 `log.Printf` + 通用 500 文案，不泄内部细节。处理器不返回 error，全部直接写响应。
- **事务模式**：`tx, _ := s.pool.Begin(ctx)` + `defer tx.Rollback(ctx)` + 成功路径显式 `Commit`；共享 helper（`inventory.AddPourTx`、`inventory.SellRetailTx`、`platform.LogOperation`、`auth.ConsumeSecondFactor`）接收 `pgx.Tx` 由调用方提交。
- **HTML 渲染**：`html/template` 自动转义，统一通过 `ui.Renderer` 注入全局字段（`Brand`、`Categories`、`Me`、`Roles`、`{{asset}}`）；独立壳模板（`pos/pos.html`、`pos/pos_pay.html`、`screen/screen.html`）通过 `RenderShell` 渲染，其余套 `templates/layouts/admin.html`；路由 pattern 用 Go 1.22 方法+通配（`POST /pos/order/{id}/item/{item}/less`）。
- **注释**：解释“为什么不能这么做”的中文注释（如迁移事务放不了 `create index concurrently`、`t.Fatal` 只能在主 goroutine）。
- **命名**：处理器直白动词（`PosPay`、`TapBeerHandler`、`RetailSave`）；测试 `Test<不变量>`（如 `TestPayOrderAtomicAndIdempotent`），DB 文件后缀 `_integration_test.go`。
- **副作用通知**：涉及酒单可见状态、进货/开结桶、POS 结账或零售低库存的写操作，在 `tx.Commit` **成功后**通过 `event.EventBus.Publish(...)` 发布领域事件（如 `EventScreenChanged` 触发 `screen.Hub.Broadcast()` 与异步 Webhook 投递）。

## Important Files

|文件|为什么重要|
|---|---|
|`cmd/pourops/main.go`|CLI 入口：env 读取、DB 迁移与 BOSS 初始化、优雅关停|
|`internal/app/app.go`|应用组合根：`Deps`、`NewAssets`、`NewRenderer`、`NewHandler`（8 服务装配与公共中间件）|
|`internal/platform/db.go`|迁移锁机制、`Migrate` 与并行隔离库 `NewTestPool`|
|`internal/event/{bus,webhook}.go`|进程内 `EventBus`、HMAC-SHA256 异步 `WebhookDispatcher` 与 SSRF 校验|
|`internal/ui/{layout,renderer,assets}.go`|布局 ViewModel、`Renderer`、`AssetManifest`（`/static/css/admin.bundle.css` 拼装与内容指纹）|
|`internal/settings/{store,http}.go`|`SettingsStore` 并发安全参数缓存与 BOSS 专属 `/admin/settings`|
|`internal/pos/{store,gateway}.go`|`PayItemsWithGateway` 结账事务（全系统资金入口）、`lockOpenOrder`、`EnsureOrder`、`PaymentGateway` / `ReceiptPrinter` 接口|
|`internal/inventory/{keg,economics,retail}.go`|`AddPourTx`、开/结桶状态机、可配置出酒/毛利测算、`SellRetailTx` 扣库存+自动下架+低库存识别|
|`internal/catalog/{beer,store,poster,codec}.go`|酒款/历史酒单持久化、本地酒标存储与旧图回收、ZIP/CSV/JSON `Codec`|
|`internal/lookup/provider.go`|多源酒款检索聚合接口与故障降级调度|
|`internal/auth/{auth,security}.go`|会话签名（`session|`/`challenge|`/`2fa-setup|` 域隔离）、三种守卫、限速、TOTP/AES-GCM/恢复码、`ConsumeSecondFactor`|
|`web/embed.go`|前端模板 `TemplatesFS` 与静态资源 `StaticFS` 唯一嵌入声明|

## Runtime/Tooling Preferences

- 运行时：Go 1.24（唯一直接依赖 `github.com/jackc/pgx/v5`）；PostgreSQL 14–17。
- 前端零构建：无 npm/package.json；后台使用内嵌的 Bootstrap / Bootstrap Icons / Chart.js（`web/static/vendor/`）；电视页原生 JS 无外部依赖。
- 环境变量（`platform.Env()` 把空串当未设）：

|变量|默认|要点|
|---|---|---|
|`DATABASE_URL`|`postgres://localhost:5432/bar?sslmode=disable`|pgxpool DSN|
|`ADDR`|`:8080`|监听地址|
|`BRAND`|`PourOps`|门店默认品牌（可被 `store_settings` 覆盖）|
|`POSTER_DIR`|`posters`|本地酒标图片存储目录（`/posters/{name}`）|
|`BOSS_PASSWORD`|无|仅 users 表为空时读取，≥6 位，不写日志|
|`SESSION_SECRET`|进程随机|不设则每次重启全员掉线|
|`TWO_FACTOR_KEY`|无|32 字节 hex（`openssl rand -hex 32`）；缺失不能开新 2FA、已开不降级；换了它恢复码仍可用|
|`JIUHE_API_BASE`|`https://mall.jiuhehe.com/api`|远端酒款检索服务地址|
|`TEST_DATABASE_URL`|无|测试专用，未设跳过 DB 用例|

## Testing & QA

- **单元与 DB 集成不靠 build tag**：`platform.NewTestPool(t, migrations.FS)`（`internal/platform/db.go`）读 `TEST_DATABASE_URL`，空则 `t.Skip`；每个测试建独立 schema `test_<pid>_<seq>` 并自动跑全部迁移，`t.Cleanup` drop。同一 DSN 并行跑多个 `go test` 进程安全。
- 断言纯标准库 `testing`（无 testify）：`t.Fatalf` 中文消息带实际值；表驱动为主；`t.Setenv` 注入环境，不共享全局状态。
- **`t.Fatal` 禁止在子 goroutine 里调**（会 panic）：并发用例先在主 goroutine 算好参数，错误走 `chan error` 汇总——`internal/pos/checkout_integration_test.go` 是模板。
- 各领域测试位于对应 `internal/<domain>/` 包内，`internal/app/app_test.go` 负责全 19 个模板（`ui.DefaultPageFiles`）的真实渲染（含 `{{asset}}` 指纹）与全路由冒烟测试。
- 覆盖重点：结账原子性/幂等/并发/网关失败回滚、库存不足整单回滚、外单行 id 混入被忽略、开桶幂等、流水快照、2FA 重放/恢复码/封锁、迁移回滚幂等、`EventBus` + Webhook 签名与 SSRF 防护、可配置核算参数联动。
- JS 与电视页回归：`node web/tests/keg_pricing_test.mjs`（6 组滑块与自定义默认值回归）与 `node web/tests/screen_test.mjs`（8 组电视酒单回归）。
- 验收新 UI/流程：本机起服务 + 浏览器真实操作（登录→操作→看结果），不造页面状态。
