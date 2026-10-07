# 城市租房押金托管·退租验房·争议服务

租客与房东签约后，本服务把**房源、合同期限、押金金额、托管账户、交割照片、家具家电清单、
水电表读数、维修责任**记录在同一条租赁档案上；入住期间关联维修工单；退租时按合同与入住交割
材料生成**可扣款项建议**；对墙面损坏 vs 自然磨损、家电维修责任不清、水电欠费、钥匙丢失、
房东迟迟不验房、租客提前退租、清洁费用争议，统一走证据驱动的争议流程；**只有争议全部闭合、
扣款全部定论、双方确认结算后，押金才会释放**。

**退租水电尾款结算**：租客搬离时最后一期供应商水电账单尚未生成，平台按搬离最近读数与历史
月均用量先从押金中预留一笔（独立第四层资金 `utility_hold`），并把**预留原因与预计结算时间**
展示给双方；账单到达后，系统把**供应商账单读数与租客搬离当天水表/电表照片读数放在一起复核**，
不能只按账单金额扣款——账单延迟、读数无法对应、双方对费用分摊有争议时，预留款保持待处理并
进入**人工复核**；复核通过后**多退少补**（多预留的退回、不足部分向租客发起补缴）。同一条
租赁**只产生一笔最终押金流水**，重复收到同一账单或补传照片都不会重复扣款，托管账户始终与
这笔最终结果一致。

技术栈：**Go 1.19 + PostgreSQL 15 + Redis 7 + Kafka 3.7(KRaft)**。后端服务，无业务前端页面；
接口文档由内嵌 **Swagger UI / OpenAPI 3.x** 提供。

---

## 1. 快速开始（Docker Compose 一键启动）

```bash
docker compose up -d --build
# 服务: http://localhost:8080
# Swagger UI: http://localhost:8080/swagger/
# OpenAPI:    http://localhost:8080/openapi.yaml
```

容器包含 app / postgres / redis / kafka(KRaft 单节点)。app 启动时自动执行数据库迁移并写入
演示账号，Kafka 主题 `lease.events` 自动创建。

停止并清理数据卷：

```bash
docker compose down -v
```

## 2. 本地开发（不使用 Docker）

依赖：Go 1.19+、PostgreSQL、Redis、Kafka（KRaft 单节点即可）。

```bash
# 1) 准备数据库
createdb deposit
psql "$DATABASE_URL" -f migrations/0001_init.sql     # 也可让服务启动时自动迁移

# 2) 启动服务（环境变量带默认值，见 internal/config/config.go）
export DATABASE_URL='postgres://app@127.0.0.1:5432/deposit?sslmode=disable'
export REDIS_ADDR=127.0.0.1:6379
export KAFKA_BROKERS=127.0.0.1:9092
go run ./cmd/server
# 无 Kafka 环境时可 KAFKA_DISABLED=true（事件仍写入 lease_events 表，只是不投递）
```

健康检查：

```bash
curl localhost:8080/healthz   # 存活
curl localhost:8080/readyz    # Postgres / Redis / Kafka 依赖状态
```

## 3. 测试账号（固定演示 Token）

服务启动时 `SEED=true`（默认）会创建 4 个用户并把固定会话写入 Redis，直接可用：

| 角色   | 手机号        | 固定 Bearer Token | 说明                 |
|--------|---------------|-------------------|----------------------|
| 租客   | 13800000001 | `tenant-token`    | 小李                 |
| 房东   | 13800000002 | `landlord-token`  | 老王                 |
| 中介   | 13800000003 | `agent-token`     | 张姐                 |
| 平台   | 13800000004 | `platform-token`  | 客服/裁决方          |

也可以用手机号登录换取随机 token：

```bash
curl -X POST localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' -d '{"phone":"13800000001"}'
```

所有业务接口携带 `Authorization: Bearer <token>`。另有一套已登记的演示房源
（北京·建国路 88 号），可直接拿来签约。

## 4. 验证方式

### 4.1 自动化验证脚本（推荐）

脚本需要能访问服务的 `curl`，以及能连数据库的 `psql`（用于断言资金状态）。
数据库连接通过环境变量配置（默认适配本地开发实例 127.0.0.1:5433）：

```bash
# 本地开发默认(PG5433)
bash scripts/e2e.sh
# Docker Compose 环境
PGPORT=5432 PGUSER=app PGPASSWORD=app PGDATABASE=deposit bash scripts/e2e.sh
```

```bash
# 主场景（有 Kafka 时 41 个断言；无 Kafka 时 39 断言 + 2 SKIP）：
# 签约→托管→入住交割→维修定责→退租验房→
# 墙面争议→平台裁决→双方确认放款，并校验 Kafka 事件投递与 DB 触发器闸门
bash scripts/e2e.sh

# 补充争议场景（51 个断言）：
bash scripts/e2e-scenarios.sh

# 退租水电尾款场景（75 个断言）：
bash scripts/e2e-utility.sh
```

### 4.2 单元测试

```bash
go test ./...
```

`internal/rules` 含 14 个规则引擎用例：自然磨损 0 元、入住前损坏免责、入住基线 GOOD 新损不免责、
租客/房东/共担维修责任、家电责任不清、提前退租通知期、清洁/欠费/钥匙合计、押金上限截断等。

`internal/app` 另含 9 个水电尾款复核纯函数用例：无照片按账单、照片账单一致按账单、容差内对应、
账单延迟挂起、账单读数低于搬离读数不扣款、照片与账单矛盾转人工（不按账单扣款）、分摊争议最高
优先级、以及预留金额估算。

### 4.3 手动走一遍主流程

```bash
T=tenant-token; L=landlord-token
PID=$(psql ... -tAc "SELECT id FROM properties LIMIT 1")
TID=$(psql ... -tAc "SELECT id FROM users WHERE phone='13800000001'")

# 签约 → 租客充押金托管
curl -X POST localhost:8080/api/v1/contracts -H "Authorization: Bearer $L" \
  -H 'Content-Type: application/json' -d "{\"property_id\":\"$PID\",\"tenant_id\":\"$TID\",
  \"start_date\":\"2026-01-01\",\"end_date\":\"2026-12-31\",\"rent_amount\":5000,
  \"deposit_amount\":10000,\"inspection_due_days\":7}"
CID=<返回的 id>
curl -X POST localhost:8080/api/v1/contracts/$CID/deposit -H "Authorization: Bearer $T"

# 任意时刻查看同一条租赁的完整档案（合同/托管/台账/交割/维修/验房/扣款/争议/事件）
curl -H "Authorization: Bearer $T" localhost:8080/api/v1/contracts/$CID/timeline
```

其余步骤（入住交割、报修定责、交房、验房提交、逐项接受/申诉、争议动作、结算确认）请直接在
Swagger UI 点击调用，或参考两个脚本里的请求体。

## 5. 核心设计：资金状态 / 争议状态 / 证据状态如何保持一致

### 5.1 押金的三层金额（退租水电预留为第四层，见 5.6）

押金进入托管后只是资金状态，真正决定释放的是证据与争议。`escrow_accounts` 维护：

| 字段            | 含义                                                       |
|-----------------|------------------------------------------------------------|
| `balance`       | 在管押金总额（含租客后续补缴的水电尾款）                   |
| `disputed_hold` | **软冻结**：验房建议/争议中的扣款，任何一方都拿不到         |
| `hard_hold`     | **定论预留**：租客接受、协商一致或平台裁决支持的扣款        |
| `utility_hold`  | **水电尾款预留**：最后一期账单未出时圈定，结算前不得释放    |
| 约束            | `hard_hold + disputed_hold + utility_hold <= balance`      |

每次变动同时追加 append-only 的 `fund_ledger`（充值/建议冻结/转定论/解冻/扣款/退还/
水电预留/水电最终结算/租客补缴）。

### 5.2 扣款建议不是扣款

`POST /inspection/submit` 调用纯函数规则引擎（`internal/rules`），结合合同条款、入住交割基线、
家具家电清单、维修责任工单、退租六维结论（墙/地板/家电/钥匙/水电/清洁）生成建议：

- 自然磨损、入住前已损坏/磨损、房东责任维修 → 产出 **0 元保护性建议**；
- 家电没有任何维修记录或工单责任未定 → `APPLIANCE_RESPONSIBILITY_UNCLEAR`，要求先澄清责任；
- 租客责任维修按 `tenant_share`，共担按 50%；无前置记录的损坏给房东报价但允许租客申诉自然磨损；
- 提前退租按合同 `notice_days` 与月租测算空置损失；
- 清洁/欠费/钥匙按报价与读数差生成费用主张；**合计以押金为上限截断**。

建议金额只进 `disputed_hold`。租客**逐项**接受才转 `hard_hold`；申诉则打开争议，款项不动。

### 5.3 争议与资金的同事务联动

所有写操作都在一个数据库事务内，对 `contract_id` 取
`pg_advisory_xact_lock(hashtextextended(...))` 串行化：

- 租客申诉 → 合同 `DISPUTING`，款项保持 `disputed_hold`；
- `PARTIES_AGREE`（协商金额）/`ADJUDICATE`（平台裁决金额）/`WITHDRAW`（撤回按原主张）闭合争议：
  支持房东的部分 `disputed_hold → hard_hold`，未支持部分**立即解冻**回可退余额；
  扣款项同步进入 `ACCEPTED / UPHELD / OVERTURNED / REJECTED` 终态；
- 全部争议闭合后合同回到 `DEDUCTION_PROPOSED`。

### 5.4 数据库级的"未闭合不得放款"闸门

`settlements` 上有触发器 `settlement_guard`：结算单从 PROPOSED 转 CONFIRMED 时，数据库自己检查
(1) 不存在 OPEN/EVIDENCE_PENDING/UNDER_REVIEW 争议；(2) 结算扣款额 == `hard_hold`。
不满足直接抛异常回滚事务——**应用层被绕过也无法先放款**。业务层还在生成结算单时提前拦截
仍有 `disputed_hold` 的情况（409 `UNRESOLVED_DISPUTE`）。结算必须租客与房东双方确认，
第二次确认在同一事务里执行 `DEDUCT`（打给房东）+ `RELEASE_TO_TENANT`（退还余额），合同进入
`RELEASED`，托管余额清零。

### 5.5 事件与通知

每个状态变化在业务事务内写 `lease_events`（事务性 outbox）。后台 relay
（`internal/events`）每 500ms 用 `FOR UPDATE SKIP LOCKED` 取出未发布事件，以 `contract_id`
为 key 发往 Kafka（同一租赁同一分区、严格有序），发送成功才标记 published。语义为
at-least-once，事件携带 `event_id`，消费方须幂等。关键节点同时写 `notifications` 表。

### 5.6 退租水电尾款预留（第四层资金）

租客搬离、供应商最后一期账单尚未出具时，水电费用无法立刻定论，于是托管账户在
`balance / hard_hold / disputed_hold` 之外增加第四层：

| 字段 | 含义 |
|------|------|
| `utility_hold` | **水电尾款预留**：按搬离最近读数 + 历史月均用量圈定，账单结算前任何一方都拿不到 |
| 约束 | `hard_hold + disputed_hold + utility_hold <= balance` |

流程（`internal/app/utility.go`，复核纯函数在 `utility_review.go`）：

1. **创建预留** `POST /utility-reserve`（房东/中介/平台，交房后）：金额=Σ（退租读数−入住读数）
   ÷租住月数（不足一月按一月）×单价；记录 `reason`（账单尚未生成）与
   `expected_settle_at`（搬离日 + N 天，默认 30），双方都能在 `GET /utility-reserve` 与
   `timeline.utility_reserve` 看到。若验房已生成"水电欠费"待确认建议，由预留接管并释放其
   `disputed_hold`；已定论扣款（ACCEPTED/UPHELD）则拒绝预留，避免双重扣款。
2. **搬离照片** `POST /utility-reserve/photos`（租客）：上传搬离当天水表/电表照片证据及读数，
   每品类一条、补传覆盖；**最终结算后禁止补传**，同一证据重复提交幂等。
3. **账单 × 照片复核** `POST /utility-reserve/bills`：账单按 `供应商+账单号` 幂等。纯函数
   `ReviewUtilityItems` 把账单读数与搬离最近读数、照片读数放在容差（1 个表计单位）内比对：
   - 任一分项存在未闭合分摊争议 → `SPLIT_DISPUTE`；账单缺失 → `BILL_DELAY`；账单读数低于
     搬离读数、或与照片读数超容差 → `READING_MISMATCH`——三种情况都**不扣款**，预留款分别
     留在 `REVIEW_PENDING` / `MANUAL_REVIEW`，等待账单或平台人工复核；
   - 全部一致才定论，**有照片时以照片与账单双重吻合为依据**（`final_decision=PHOTO`），
     绝不只按账单金额扣款。
4. **多退少补**：`UTILITY_FINAL` 是**全租赁唯一的一笔最终押金流水**（部分唯一索引
   `uq_fund_ledger_one_utility_final` + 应用层双重兜底）。实际费用 `<` 预留的部分随
   `utility_hold` 释放回可退余额；`>` 预留的部分记入 `shortage_amount`，租客经
   `POST /utility-reserve/topup` 补缴（只能补到差额，多补被拒）。
5. **闸门**：`settlement_guard` 触发器新增 `assert_utility_reserve_final`——预留未
   FINALIZED、或补缴未到账时，结算单转 CONFIRMED 直接抛 `CONFLICT_UTILITY_PENDING` /
   `CONFLICT_UTILITY_TOPUP` 回滚；应用层在生成结算单时同样提前 409。最终放款后托管余额清零。
6. **争议**：分摊/读数争议复用 `disputes`（新原因码 `UTILITY_FINAL_SETTLEMENT`），预留立即
   转 `MANUAL_REVIEW`；平台 `ADJUDICATE` 按定论费用把争议品类分摊、未争议品类按各自账单结算，
   闭合后同样只走唯一的 `UTILITY_FINAL` 流水。

## 6. 项目结构

```
cmd/server/            启动入口(迁移/seed/relay/HTTP)
migrations/            SQL 迁移(0001 表/约束/结算守卫; 0002 水电尾款预留第四层资金+最终流水幂等)
internal/
  config/              环境变量配置
  db/                  PG 连接池与迁移执行
  domain/              领域模型与状态常量
  rules/               退租扣款建议规则引擎(纯函数, 带单测)
  app/                 事务用例: 签约/托管/交割/维修/验房/争议/结算/资金台账/水电尾款预留/查询/seed
                       - utility.go          预留/账单/照片/争议/人工复核/补缴用例
                       - utility_review.go   账单×照片复核纯函数(带单测)
  events/              lease_events outbox -> Kafka relay
  auth/                Redis 会话 Token
  api/                 HTTP 路由、处理器、Swagger 静态资源
web/
  openapi.yaml         OpenAPI 3.x 规范(31 个路径)
  swagger-ui/          内嵌 Swagger UI 静态资源(离线可用)
scripts/
  e2e.sh               主生命周期端到端验证(39 断言 + 无 Kafka 时 2 项 SKIP, 含 DB 触发器闸门与跨租户隔离)
  e2e-scenarios.sh     7 类争议场景验证(51 断言)
  e2e-utility.sh       退租水电尾款预留场景验证(75 断言)
  e2e-lib.sh           脚本公共函数
Dockerfile / docker-compose.yml
```

## 7. 合同状态机

```
DRAFT ─签约→ SIGNED ─押金入托管→ LIVING ─退租交房→ CHECKOUT_PENDING
                                   │                 └─提前退租→ EARLY_TERMINATED
                                   └──────────────────────────────┐
                                                                   ▼
                                          INSPECTING ←─ 租客要求复验(REINSPECT)
                                               │
                                               ▼
                                      DEDUCTION_PROPOSED ◄──────┐
                                      │ 租客逐项接受/申诉          │ 争议全部闭合
                                      ▼                           │
                                     DISPUTING ─协商/裁决/撤回─────┘
                                               │
                                               ▼
                                      结算 PROPOSED → CONFIRMED(双方+触发器校验)
                                               │
                                               ▼
                                          RELEASED (余额退还租客 / hard_hold 付房东)
```

## 8. 已执行的验证结论

以下验证在本次交付环境中实际执行通过（Go 1.19、本机用户级 PostgreSQL 15 :5433、Redis :6379、
`KAFKA_DISABLED=true`，环境无 Kafka broker 与 Docker daemon）：

- `go vet ./...` 无告警；`go build ./...` 通过；`go test ./...`：规则引擎 14 个用例 +
  水电复核 9 个用例全部通过。
- `scripts/e2e.sh` **39/39** 断言通过（Kafka 不可达时 2 项投递断言自动 SKIP），覆盖：
  权限隔离、水电煤读数差自动计费（265.00 元）、规则建议金额、`disputed_hold`/`hard_hold` 联动、
  **争议打开时生成结算被 409 拒绝**、平台裁决部分支持（800→500）后的资金拆分、双方确认放款。
- `scripts/e2e-scenarios.sh` **51/51** 断言通过，覆盖 7 类争议：
  自然磨损/入住前损坏免责、家电责任不清裁决全额驳回（OVERTURNED）、清洁费协商折价、
  提前退租空置损失、钥匙丢失申诉撤回、房东逾期不验房申诉+平台巡检自动代开验房并自动闭合争议。
- `scripts/e2e-utility.sh` **75/75** 断言通过，覆盖退租水电尾款结算全部分支：
  预留原因/预计结算时间展示与第四层 `utility_hold` 圈定；搬离照片与账单一致才扣款（多退 30）；
  **重复账单（`reused=true`）与重复补传照片幂等，全程只有 1 笔 `UTILITY_FINAL` 流水**；
  验房不再重复生成水电欠费主张；账单读数与照片矛盾→`READING_MISMATCH` 挂起人工复核、
  应用层与 `settlement_guard` 触发器双重阻止放款、平台人工定论 55 后只产生 1 笔最终流水；
  分摊争议→`SPLIT_DISPUTE` 保持待处理、平台裁决 40 后结算；实际费用超预留→补缴 90，
  **补缴未到账时生成结算 409、DB 触发器抛 `CONFLICT_UTILITY_TOPUP`**，房东代缴 403、
  超补 409，补缴后托管 `balance=10090 / hard_hold=240`、放款后清零且 `released_total=10090`。
- OpenAPI 规范经 yaml.v3 解析通过（**31 个路径、19 个 schema**），`/swagger/`、
  `/openapi.yaml`、`/healthz`、`/readyz` 均返回 200/正常。

> 说明：交付环境没有 Docker daemon，因此 `docker compose up` 未在本机实际执行；
> Dockerfile 为标准多阶段 Go 构建，compose 中 postgres/redis/kafka 均使用官方镜像与健康检查，
> 请在具备 Docker 的环境按第 1 节启动验证（compose 内 app 会带上 Kafka，e2e 的投递断言也会执行）。
