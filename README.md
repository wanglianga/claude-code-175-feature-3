# 城市租房押金托管·退租验房·争议服务

租客与房东签约后，本服务把**房源、合同期限、押金金额、托管账户、交割照片、家具家电清单、
水电表读数、维修责任**记录在同一条租赁档案上；入住期间关联维修工单；退租时按合同与入住交割
材料生成**可扣款项建议**；对墙面损坏 vs 自然磨损、家电维修责任不清、水电欠费、钥匙丢失、
房东迟迟不验房、租客提前退租、清洁费用争议，统一走证据驱动的争议流程；**只有争议全部闭合、
扣款全部定论、双方确认结算后，押金才会释放**。

**退租水电尾款**：租客搬离时最后一期水电账单往往还没出，平台先按最近读数与历史用量
**预留一部分押金**，把预留原因与预计结算时间展示给双方；账单到达后登记供应商账单（账单号
幂等去重），租客可补传**搬离当天的水表/电表照片**，平台把照片读数与账单**逐项交叉复核**后
才生成**唯一一笔最终押金流水**——多退少补，不足部分向租客发起补缴；账单延迟、读数无法
对应或分摊有争议时预留款保持**人工复核**待处理，重复账单/重复补照片/重复结算都不会
重复扣款，托管账户与最终结果同事务勾稽。

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
# 主场景（41 个断言）：签约→托管→入住交割→维修定责→退租验房→
# 墙面争议→平台裁决→双方确认放款，并校验 Kafka 事件全部投递与 DB 触发器闸门
bash scripts/e2e.sh

# 补充争议场景（51 个断言）：
bash scripts/e2e-scenarios.sh

# 退租水电尾款场景（103 个断言）：
# 预留展示→账单幂等→照片交叉复核→多退少补/补缴→账单延迟/分摊争议/读数无法对应人工复核→
# 唯一一笔最终流水与托管勾稽→主押金结算放行
bash scripts/e2e-utility.sh
```

### 4.2 单元测试

```bash
go test ./...
```

`internal/rules` 含 15 个规则引擎用例：自然磨损 0 元、入住前损坏免责、入住基线 GOOD 新损不免责、
租客/房东/共担维修责任、家电责任不清、提前退租通知期、清洁/欠费/钥匙合计、押金上限截断、
**已做水电尾款预留后验房不再生成水电欠费建议（防双重扣款）** 等。

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

### 5.1 押金的三层金额

押金进入托管后只是资金状态，真正决定释放的是证据与争议。`escrow_accounts` 维护：

| 字段            | 含义                                                       |
|-----------------|------------------------------------------------------------|
| `balance`       | 在管押金总额                                               |
| `disputed_hold` | **软冻结**：验房建议/争议中的扣款，任何一方都拿不到         |
| `hard_hold`     | **定论预留**：租客接受、协商一致或平台裁决支持的扣款        |
| 约束            | `hard_hold + disputed_hold <= balance`                      |

每次变动同时追加 append-only 的 `fund_ledger`（充值/建议冻结/转定论/解冻/扣款/退还）。

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

### 5.6 退租水电尾款：预留 → 账单/照片交叉复核 → 唯一一笔最终流水

租客搬离当天供应商最后一期账单还没出。流程与不变量（迁移 `0002_utility_reservation.sql`）：

1. **预留展示给双方**：`POST /utility-reservation`（房东/中介/平台）按最近表计读数与历史月均
   用量自动估算（默认预留 2 个月月均，也可显式指定），从当前**未被占用**的押金余额中预留，
   资金直接进入 `hard_hold`（台账 `UTILITY_RESERVE`）。预留原因 `reason` 与预计结算时间
   `expected_settle_at`、分项估算口径 `estimate_breakdown` 都挂在 timeline 上对租客和房东可见。
   一份合同至多一笔预留（UNIQUE）；已预留后验房规则引擎**不再生成 `UTILITY_ARREARS` 建议**，
   防止双重扣款。
2. **供应商账单（账单号幂等）**：`utility_bills` 对 `(contract_id,bill_no)` 建唯一约束，
   重复收到同一账单只返回原记录（HTTP 200 + `duplicated:true`），不改状态、不动资金。
   无租客照片时预留进入 `BILL_RECEIVED`；租客已补传照片则进入 `REVIEW_PENDING`。
3. **照片读数必须与账单一起复核**：租客通过 `meter-photos`（证据须为本合同 `UTILITY` 类）
   补传搬离当天水/电表读数，同类表重传只替换最新版（旧读数 `superseded` 留痕）。平台调用
   `/review` 逐项并排核对：`MATCH` 按账单、`ADOPT_PHOTO` 以照片读数×单价重算（核定金额回写
   账单 `settled_amount`，账单原值保留）、`UNMATCHABLE` 读数无法对应则转 `MANUAL_REVIEW`。
   **待复核状态下数据库触发器直接拒绝最终结算，不可能只按账单金额扣款。**
4. **人工复核**：账单延迟（平台巡检 `POST /api/v1/admin/utility-bill-delay` 置
   `BILL_DELAYED`）、读数无法对应（`READING_UNMATCHABLE`）、双方对分摊有争议
   （`/dispute` 打开 `UTILITY_FINAL_SETTLEMENT` 争议，原因 `ALLOCATION_DISPUTE`）时，
   预留款保持 `MANUAL_REVIEW` 待处理；分摊争议未闭合或平台未执行 `/manual-resolve` 前，
   水电最终结算与**主押金结算确认都会被阻止**（`settlement_guard` 增加未结清预留闸门）。
   供应商始终不出账时，平台可在 `manual-resolve` 中给出核定金额，系统自动生成平台核定账单。
5. **唯一一笔最终押金流水（多退少补）**：`/finalize` 以核定金额 `charge` 结算：
   `from_deposit=MIN(charge,reserved)` 从预留支付供应商/房东，
   `refund=reserved-from_deposit` 退还租客，`supplement=MAX(charge-reserved,0)` 形成
   `utility_supplements` 补缴应收（**不入托管**，租客支付，重复支付幂等）。
   台账只追加 `UTILITY_FINAL_SETTLE`（支付/退款各一笔，勾稽和恰好等于预留额），
   `final_utility_settlements` 对合同与预留均 UNIQUE，金额勾稽由列 CHECK + 触发器双重保证。
   重复调用 `/finalize`、重复收到同一账单、结算后补传照片都只返回既有结果（`idempotent:true`），
   **绝不产生第二笔扣款**；托管三层金额与最终结果在同一事务（合同级 advisory lock）内一致。
   水电结清后主押金结算单把已付/已退金额并入 `deducted_total`/`returned_to_tenant`，
   仍满足 `扣款+退还=押金`，双方确认后合同 RELEASED、托管余额清零。

## 6. 项目结构

```
cmd/server/            启动入口(迁移/seed/relay/HTTP)
migrations/            SQL 迁移(表、约束、索引、结算守卫触发器)
  0001_init.sql         主流程(合同/托管/交割/维修/验房/争议/结算)
  0002_utility_reservation.sql  退租水电尾款预留/账单/照片复核/唯一最终流水/补缴
internal/
  config/              环境变量配置
  db/                  PG 连接池与迁移执行
  domain/              领域模型与状态常量
  rules/               退租扣款建议规则引擎(纯函数, 带单测)
  app/                 事务用例: 签约/托管/交割/维修/验房/争议/结算/水电尾款/资金台账/查询/seed
  events/              lease_events outbox -> Kafka relay
  auth/                Redis 会话 Token
  api/                 HTTP 路由、处理器、Swagger 静态资源
web/
  openapi.yaml         OpenAPI 3.x 规范(34 个路径)
  swagger-ui/          内嵌 Swagger UI 静态资源(离线可用)
scripts/
  e2e.sh               主生命周期端到端验证(41 断言, 含 DB 触发器闸门与跨租户隔离)
  e2e-scenarios.sh     7 类争议场景验证(51 断言)
  e2e-utility.sh       退租水电尾款预留/复核/最终结算验证(103 断言)
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

水电尾款预留（独立于主状态机，资金并入 escrow 的 `hard_hold`；未结清时主结算不得确认）：

```
RESERVED ──供应商账单(无照片)──► BILL_RECEIVED ──finalize──► SETTLED (一笔最终押金流水: 多退少补)
   │                              ▲   │
   │              照片+账单平台复核通过│   └─租客已补照片─► REVIEW_PENDING ──/review(MATCH|ADOPT_PHOTO)─┘
   │                              │
   ├─账单延迟(BILL_DELAYED)─┐      ├─读数无法对应(UNMATCHABLE)─► MANUAL_REVIEW
   └─分摊争议/读数无法对应──┴────► (READING_UNMATCHABLE/ALLOCATION_DISPUTE)
                                        │ 平台 manual-resolve(争议须先闭合;
                                        │ 无账单可给核定金额, 自动补平台核定单)
                                        └──────────────────────────────► BILL_RECEIVED
```

## 8. 已执行的验证结论

以下验证在交付环境中实际执行通过：

- `go vet ./...` 无告警；`go build ./...` 通过；`go test ./...` 规则引擎 15 个用例全部通过。
- 使用本机用户级 PostgreSQL 15（端口 5433，`initdb -U app --auth=trust`）、本机 Redis 7、
  `KAFKA_DISABLED=true` 启动服务（事件仍写入 `lease_events` outbox），
  `scripts/e2e.sh` **39/39 业务断言通过**（另 1 项 Kafka 投递断言因本机无 Kafka 跳过，
  outbox `published` 断言同原因未满足），覆盖：权限隔离、水电读数差计费、规则建议金额、
  `disputed_hold`/`hard_hold` 联动、争议打开时结算被 409 拒绝、平台裁决部分支持后的资金拆分、
  双方确认放款与余额清零。
- `scripts/e2e-scenarios.sh` **51/51** 断言通过，覆盖 7 类争议：
  自然磨损/入住前损坏免责、家电责任不清裁决全额驳回（OVERTURNED）、清洁费协商折价、
  提前退租空置损失、钥匙丢失申诉撤回、房东逾期不验房申诉+平台巡检自动代开验房并自动闭合争议。
- `scripts/e2e-utility.sh` **103/103** 断言通过，覆盖退租水电尾款全链路：
  自动估算预留（区间月均×2=480）进入 `hard_hold` 且双方 timeline 可见预留原因/预计结算时间、
  重复预留 409、已预留后验房不再产出水电欠费建议（防双重扣款）、账单号重复上传幂等不扣款、
  无照片按实账结算（90 元，多预留 390 退还）、租客补传搬离当天照片后必须交叉复核、
  未复核结算 409、平台 `ADOPT_PHOTO` 按照片读数重算（账单 300 核定为 74）、
  实际费用超预留生成补缴（60 元，房东不得代缴、重复支付幂等）、
  账单延迟巡检转 `MANUAL_REVIEW` 后平台核定 80 元（自动补平台核定单，结算口径
  `MANUAL_ADJUDICATED`）、分摊争议打开时水电与主押金结算均被 409 拒绝（含绕过应用层的
  DB 触发器兜底验证）、读数无法对应转人工后恢复结算、**每份合同仅一笔最终结算**、
  `UTILITY_FINAL_SETTLE` 台账金额之和恰好等于预留额、主押金结算并入已付/已退后
  托管累计释放=押金全额（余额清零）。
- OpenAPI 规范可解析（34 个路径、25 个 schema，含 9 个 utility 路径），
  `/swagger/`、`/swagger/swagger-ui.css`、`/openapi.yaml` 均返回 200；`/readyz` 返回
  `{"postgres":"ok","redis":"ok","kafka":"disabled"}`。水电相关 9 类领域事件均写入
  `lease_events` outbox（UtilityReservationCreated / UtilitySupplierBillReceived /
  UtilityMeterPhotosSubmitted / UtilityMeterEvidenceReviewed / UtilityFinalSettled /
  UtilitySupplementPaid / UtilityBillDelayed / UtilityAllocationDisputeOpened /
  UtilityManualReviewResolved）。

> 说明：交付环境没有 Docker daemon 且无法下载 Kafka 发行包，因此 `docker compose up` 与
> Kafka console-consumer 投递断言未在本机实际执行；事件在无 Kafka 时仍保证事务性落 outbox，
> relay 在有 broker 时自动补发（at-least-once，消费方按 event_id 幂等）。
> Dockerfile 为标准多阶段 Go 构建，compose 中 postgres/redis/kafka 均使用官方镜像与健康检查，
> 请在具备 Docker 的环境按第 1 节启动验证。
