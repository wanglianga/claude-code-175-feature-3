#!/usr/bin/env bash
# 退租水电尾款结算 端到端验证:
#   场景1: 预留展示 -> 账单到达(无照片按实账) -> 多退 -> 唯一一笔最终流水 -> 幂等(重复账单/重复结算/
#          结算后补传照片均不重复扣款) -> 主押金结算放行, 托管勾稽; 验房不再生成水电欠费建议(防双扣)。
#   场景2: 租客补传搬离当天读表照片 -> 必须复核, 未复核禁止扣款 -> ADOPT_PHOTO 按照片读数重算 -> 少退差额。
#   场景3: 实际费用超出预留 -> 向租客发起补缴 -> 支付幂等, 房东不能代缴。
#   场景4a: 账单延迟 -> MANUAL_REVIEW 保持待处理 -> 平台核定 -> 人工定论结算。
#   场景4b: 双方分摊争议 -> MANUAL_REVIEW + 主押金结算被闸门阻止 -> 裁决闭合 -> 恢复 -> 结算。
#   场景4c: 照片读数无法对应 -> UNMATCHABLE 人工复核 -> 恢复后结算(MANUAL_ADJUDICATED)。
#   另含: 预留唯一、重复补传照片不产生第二份有效读数、DB 触发器兜底、timeline 双端可见。
# 依赖: scripts/e2e-lib.sh; 用法 PGPORT=5433 bash scripts/e2e-utility.sh
set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=e2e-lib.sh
source "$HERE/e2e-lib.sh"

TID=$(PSQL "SELECT id FROM users WHERE phone='13800000001'")
LID=$(PSQL "SELECT id FROM users WHERE phone='13800000002'")
PID=$(PSQL "SELECT id FROM properties LIMIT 1")
SEQ=$(date +%s)

# 新租约(已充押金, 已入住交割+退租交房, 带水/电读数; 水100@5 电2000@0.6 -> 退租112/2300 区间费 水60+电180=240)
# new_lease <rent> <deposit> [no_meters]
new_lease() {
  local rent=$1 dep=$2
  local body code cid
  body=$(printf '{"property_id":"%s","tenant_id":"%s","start_date":"2026-01-01","end_date":"2026-12-31","rent_amount":%s,"deposit_amount":%s,"lease_no":"LU%s%s","notice_days":30,"inspection_due_days":7}' \
    "$PID" "$TID" "$rent" "$dep" "$SEQ" "${RANDOM}")
  req POST /api/v1/contracts "$L" "$body"; cid=$(echo "$REQ_BODY" | jqr id)
  req POST "/api/v1/contracts/$cid/deposit" "$T"
  req POST "/api/v1/contracts/$cid/checkin" "$L" '{
    "keys_handover":"钥匙2把门禁2张",
    "meters":[
      {"kind":"WATER","reading":100,"unit_price":5},
      {"kind":"ELECTRIC","reading":2000,"unit_price":0.6}]}'
  req POST "/api/v1/contracts/$cid/checkout" "$T" '{
    "keys_returned":"钥匙2把门禁2张",
    "meters":[
      {"kind":"WATER","reading":112,"unit_price":5},
      {"kind":"ELECTRIC","reading":2300,"unit_price":0.6}]}'
  echo "$cid"
}

# 登记一张 UTILITY 证据并 echo id
mk_evidence() { # <cid> <sha后缀>
  local cid=$1 sfx=$2
  local sha; sha=$(printf 'water-meter-photo-%s-%s' "$cid" "$sfx" | sha256sum | awk '{print $1}')
  req POST "/api/v1/contracts/$cid/evidence" "$T" \
    "{\"category\":\"UTILITY\",\"sha256\":\"$sha\",\"size_bytes\":1024,\"storage_uri\":\"oss://meter/$sfx.jpg\",\"caption\":\"搬离当天读表\"}"
  echo "$REQ_BODY" | jqr id
}

escrow() { PSQL "SELECT $2 FROM escrow_accounts WHERE contract_id='$1'"; }
final_count() { PSQL "SELECT count(*) FROM final_utility_settlements WHERE contract_id='$1'"; }
final_settle_rows() { PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$1' AND txn_type='UTILITY_FINAL_SETTLE'"; }

########## 场景 1: 自动估算预留 + 按账单多退 + 全链路幂等 ##########
info "场景1: 预留展示 -> 实账 90 多退 390 -> 主押金结算, 全程幂等"
CID=$(new_lease 5000 10000)
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
# 自动估算: 区间 水60+电180=240, 月均 240, 预留 2 个月 = 480
req POST "/api/v1/contracts/$CID/utility-reservation" "$L" '{"expected_settle_days":14}'
assert_eq "1 创建水电尾款预留" 201 "$REQ_CODE"
RID=$(echo "$REQ_BODY" | jqr reservation_id)
[ -n "$RID" ] && ok "1 预留 id=$RID" || bad "1 预留 id 为空"
HARD=$(escrow "$CID" hard_hold)
assert_eq "1 预留金额进入 hard_hold=480" 480.00 "$HARD"
LED=$(PSQL "SELECT amount FROM fund_ledger WHERE contract_id='$CID' AND txn_type='UTILITY_RESERVE'")
assert_eq "1 台账 UTILITY_RESERVE=480" 480.00 "$LED"
# 双方都能看到预留原因与预计结算时间
req GET "/api/v1/contracts/$CID/timeline" "$T"
RSN=$(echo "$REQ_BODY" | jqr utility_reservation.reservation.reason)
EXP=$(echo "$REQ_BODY" | jqr utility_reservation.reservation.expected_settle_at)
[ -n "$RSN" ] && ok "1 租客可见预留原因" || bad "1 租客看不到预留原因"
[ -n "$EXP" ] && ok "1 租客可见预计结算时间 $EXP" || bad "1 租客看不到预计结算时间"
req GET "/api/v1/contracts/$CID/timeline" "$L"
RSN2=$(echo "$REQ_BODY" | jqr utility_reservation.reservation.reason)
[ -n "$RSN2" ] && ok "1 房东可见预留原因" || bad "1 房东看不到预留原因"
# 重复预留被拒
req POST "/api/v1/contracts/$CID/utility-reservation" "$L" '{}'
assert_eq "1 一份合同至多一笔预留(409)" 409 "$REQ_CODE"
# 验房在已预留情况下提交 UNPAID: 规则引擎不再产出水电欠费建议(防双重扣款)
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"UNPAID","cleaning_status":"CLEAN"}'
assert_eq "1 提交验房" 201 "$REQ_CODE"
TOTAL=$(echo "$REQ_BODY" | jqr deductible_total)
assert_eq "1 已预留后验房不再建议水电欠费(合计0)" 0 "$TOTAL"
UCH=$(PSQL "SELECT count(*) FROM charge_items WHERE contract_id='$CID' AND kind='UTILITY_UNPAID'")
assert_eq "1 未生成 UTILITY_UNPAID 费用项" 0 "$UCH"

# 供应商账单到达: 水(112->118)*5=30 + 电(2300->2400)*0.6=60 = 90
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/bills" "$L" '{
  "bill_no":"BILL-1-001","supplier":"北京市自来水/电网",
  "bill_period_start":"2026-12-01","bill_period_end":"2026-12-31",
  "line_items":[
    {"kind":"WATER","start_reading":112,"end_reading":118,"usage":6,"unit_price":5,"amount":30},
    {"kind":"ELECTRIC","start_reading":2300,"end_reading":2400,"usage":100,"unit_price":0.6,"amount":60}
  ],"total_amount":90}'
assert_eq "1 登记供应商账单" 201 "$REQ_CODE"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "1 无照片账单到达 -> BILL_RECEIVED" BILL_RECEIVED "$RST"
# 同一账单号重复上传 -> 幂等
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/bills" "$L" '{"bill_no":"BILL-1-001","total_amount":90}'
assert_eq "1 重复账单幂等返回(200, duplicated)" 200 "$REQ_CODE"
DUP=$(echo "$REQ_BODY" | jqr duplicated | tr "A-Z" "a-z")
assert_eq "1 duplicated=true" true "$DUP"
BN=$(PSQL "SELECT count(*) FROM utility_bills WHERE contract_id='$CID'")
assert_eq "1 重复账单未产生第二条账单" 1 "$BN"

# 最终结算: 只生成一笔最终押金流水
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L" '{"note":"按实账90元"}'
assert_eq "1 最终结算" 201 "$REQ_CODE"
FC=$(final_count "$CID"); assert_eq "1 最终结算记录=1 笔" 1 "$FC"
FROMD=$(echo "$REQ_BODY" | jqr from_deposit)
REF=$(echo "$REQ_BODY" | jqr refund_to_tenant)
SUP=$(echo "$REQ_BODY" | jqr supplement_amount)
assert_eq "1 托管内承担=90" 90 "$FROMD"
assert_eq "1 多预留退还租客=390" 390 "$REF"
assert_eq "1 无补缴=0" 0 "$SUP"
KIND=$(echo "$REQ_BODY" | jqr settle_kind)
assert_eq "1 结算口径 BILL_ONLY" BILL_ONLY "$KIND"
FSR=$(final_settle_rows "$CID")
assert_eq "1 台账最终流水两笔(支付+退款)" 2 "$FSR"
BAL=$(escrow "$CID" balance)
assert_eq "1 托管余额=9520(10000-480 已出托管)" 9520.00 "$BAL"
HARD=$(escrow "$CID" hard_hold)
assert_eq "1 预留已释放 hard_hold=0" 0.00 "$HARD"
REL=$(escrow "$CID" released_total)
assert_eq "1 累计释放=480(预留整笔出托管)" 480.00 "$REL"

# 再次结算 / 再传同一账单: 不得重复扣款
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "1 重复最终结算幂等(200)" 200 "$REQ_CODE"
IDEM=$(echo "$REQ_BODY" | jqr idempotent | tr "A-Z" "a-z")
assert_eq "1 idempotent=true" true "$IDEM"
assert_eq "1 最终结算仍=1 笔" 1 "$(final_count "$CID")"
assert_eq "1 最终台账流水仍=2 笔" 2 "$(final_settle_rows "$CID")"
# 先在主押金结算前登记好照片证据(合同 RELEASED 后证据通道也关闭)
EVID=$(mk_evidence "$CID" late)
# 主押金结算后再补传照片: 水电已最终结算, 必须拒绝且不重复扣款
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "1 水电结清后可生成主结算单" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$T"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$L"
assert_eq "1 双方确认主结算放款" 200 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/meter-photos" "$T" \
  "{\"photos\":[{\"kind\":\"WATER\",\"reading\":118,\"evidence_id\":\"$EVID\"}]}"
assert_eq "1 最终结算后补传照片被拒(不重复扣款)" 409 "$REQ_CODE"
assert_eq "1 最终结算还是=1 笔" 1 "$(final_count "$CID")"
assert_eq "1 托管余额清零" 0.00 "$(escrow "$CID" balance)"
assert_eq "1 累计释放=10000(与押金一致)" 10000.00 "$(escrow "$CID" released_total)"
assert_eq "1 主结算退租客=9910(含水电已退390)" 9910.00 "$(PSQL "SELECT returned_to_tenant FROM settlements WHERE contract_id='$CID'")"

########## 场景 2: 照片读数与账单交叉复核, 按照片重算 ##########
info "场景2: 租客补传搬离当天照片 -> 未复核禁止扣款 -> ADOPT_PHOTO 重算 300->74"
CID=$(new_lease 5000 1000)
req POST "/api/v1/contracts/$CID/utility-reservation" "$L" '{"reserved_amount":200,"expected_settle_days":14}'
assert_eq "2 显式预留 200" 201 "$REQ_CODE"
RID=$(echo "$REQ_BODY" | jqr reservation_id)
# 账单 300 先到(无照片)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/bills" "$L" '{
  "bill_no":"BILL-2-001","line_items":[
    {"kind":"WATER","start_reading":112,"end_reading":118,"usage":6,"unit_price":5,"amount":30},
    {"kind":"ELECTRIC","start_reading":2300,"end_reading":2400,"usage":100,"unit_price":0.6,"amount":270}
  ],"total_amount":300}'
assert_eq "2 账单 300 登记" 201 "$REQ_CODE"
# 租客补传照片: 水 116(账单118), 电 2390(账单2400)
EV1=$(mk_evidence "$CID" w1)
EV2=$(mk_evidence "$CID" e1)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/meter-photos" "$T" "{
  \"photos\":[
    {\"kind\":\"WATER\",\"reading\":116,\"evidence_id\":\"$EV1\",\"taken_at\":\"2026-12-31T10:00:00Z\"},
    {\"kind\":\"ELECTRIC\",\"reading\":2390,\"evidence_id\":\"$EV2\",\"taken_at\":\"2026-12-31T10:00:00Z\"}
  ]}"
assert_eq "2 补传水/电照片" 200 "$REQ_CODE"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "2 账单+照片 -> REVIEW_PENDING 待复核" REVIEW_PENDING "$RST"
# 重复补传同读数同照片: 幂等, 有效读数仍每类 1 条
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/meter-photos" "$T" "{
  \"photos\":[
    {\"kind\":\"WATER\",\"reading\":116,\"evidence_id\":\"$EV1\"},
    {\"kind\":\"ELECTRIC\",\"reading\":2390,\"evidence_id\":\"$EV2\"}]}"
assert_eq "2 重复补传同样照片幂等成功" 200 "$REQ_CODE"
ACN=$(PSQL "SELECT count(*) FROM meter_photos WHERE reservation_id='$RID' AND superseded=false")
assert_eq "2 有效照片读数仍=2 条(未重复)" 2 "$ACN"
# 未复核直接最终结算 -> 拒绝(不能只按账单金额扣款)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "2 待复核状态禁止最终结算(409)" 409 "$REQ_CODE"
# 平台交叉复核: 采用照片读数 -> 水(116-112)*5=20 电(2390-2300)*0.6=54 = 74
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/review" "$P" \
  '{"decision":"ADOPT_PHOTO","note":"照片拍摄于搬离当天且表号一致, 按照片读数重算"}'
assert_eq "2 平台 ADOPT_PHOTO 复核" 200 "$REQ_CODE"
SETA=$(PSQL "SELECT settled_amount FROM utility_bills WHERE reservation_id='$RID' AND status='RECEIVED'")
assert_eq "2 账单核定金额回写=74" 74.00 "$SETA"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "2 复核后回到 BILL_RECEIVED" BILL_RECEIVED "$RST"
# 最终结算: 74 < 预留 200 -> 退 126
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$P"
assert_eq "2 复核后最终结算" 201 "$REQ_CODE"
assert_eq "2 托管内承担=74" 74 "$(echo "$REQ_BODY" | jqr charge_total)"
assert_eq "2 退还租客=126" 126 "$(echo "$REQ_BODY" | jqr refund_to_tenant)"
assert_eq "2 结算口径 REVIEW_CROSS_CHECK" REVIEW_CROSS_CHECK "$(echo "$REQ_BODY" | jqr settle_kind)"
MR=$(PSQL "SELECT count(*) FROM meter_reviews WHERE reservation_id='$RID' AND decision='ADOPT_PHOTO'")
assert_eq "2 复核留痕=1 条" 1 "$MR"

########## 场景 3: 实际费用超出预留 -> 补缴 ##########
info "场景3: 预留100 实账160 -> 补缴60, 支付幂等, 房东不得代缴"
CID=$(new_lease 4000 1000)
req POST "/api/v1/contracts/$CID/utility-reservation" "$L" '{"reserved_amount":100}'
RID=$(echo "$REQ_BODY" | jqr reservation_id)
assert_eq "3 创建预留" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/bills" "$L" '{"bill_no":"BILL-3-001","total_amount":160}'
assert_eq "3 账单 160 登记" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "3 最终结算(不足)" 201 "$REQ_CODE"
assert_eq "3 托管承担=100" 100 "$(echo "$REQ_BODY" | jqr from_deposit)"
assert_eq "3 退还租客=0" 0 "$(echo "$REQ_BODY" | jqr refund_to_tenant)"
assert_eq "3 补缴金额=60" 60 "$(echo "$REQ_BODY" | jqr supplement_amount)"
SUPP=$(echo "$REQ_BODY" | jqr supplement_id)
[ -n "$SUPP" ] && ok "3 生成补缴单 $SUPP" || bad "3 未生成补缴单"
SST=$(PSQL "SELECT status FROM utility_supplements WHERE id='$SUPP'")
assert_eq "3 补缴单 PENDING" PENDING "$SST"
assert_eq "3 托管余额=900(预留100出托管, 补缴不经托管)" 900.00 "$(escrow "$CID" balance)"
# 房东不能代缴
req POST "/api/v1/contracts/$CID/utility-supplements/$SUPP/pay" "$L"
assert_eq "3 房东代缴被拒(403)" 403 "$REQ_CODE"
# 租客支付
req POST "/api/v1/contracts/$CID/utility-supplements/$SUPP/pay" "$T"
assert_eq "3 租客支付补缴" 200 "$REQ_CODE"
assert_eq "3 补缴单 PAID" PAID "$(PSQL "SELECT status FROM utility_supplements WHERE id='$SUPP'")"
# 重复支付幂等, 不产生第二条
req POST "/api/v1/contracts/$CID/utility-supplements/$SUPP/pay" "$T"
assert_eq "3 重复支付幂等" 200 "$REQ_CODE"
assert_eq "3 仍只有 1 笔补缴" 1 "$(PSQL "SELECT count(*) FROM utility_supplements WHERE contract_id='$CID'")"

########## 场景 4a: 账单延迟 -> 人工复核 -> 平台核定 ##########
info "场景4a: 账单延迟 MANUAL_REVIEW -> 平台核定 80 -> 多退20"
CID=$(new_lease 5000 1000)
req POST "/api/v1/contracts/$CID/utility-reservation" "$P" '{"reserved_amount":100,"bill_due_days":-1}'
assert_eq "4a 创建预留(出账时间已过)" 201 "$REQ_CODE"
RID=$(echo "$REQ_BODY" | jqr reservation_id)
req POST /api/v1/admin/utility-bill-delay "$P"
assert_eq "4a 平台账单延迟巡检" 200 "$REQ_CODE"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
MR=$(PSQL "SELECT manual_reason FROM utility_reservations WHERE id='$RID'")
assert_eq "4a 预留 MANUAL_REVIEW" MANUAL_REVIEW "$RST"
assert_eq "4a 人工原因 BILL_DELAYED" BILL_DELAYED "$MR"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "4a 人工复核中禁止最终结算(409)" 409 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/manual-resolve" "$P" \
  '{"action":"RESUME","settled_amount":80,"note":"供应商持续未出账, 按最近读数与月均核定80"}'
assert_eq "4a 平台核定并恢复可结算" 200 "$REQ_CODE"
BN=$(PSQL "SELECT bill_no FROM utility_bills WHERE reservation_id='$RID'")
case "$BN" in MANUAL-*) ok "4a 自动生成平台核定单 $BN";; *) bad "4a 缺平台核定单: $BN";; esac
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$P"
assert_eq "4a 核定后最终结算" 201 "$REQ_CODE"
assert_eq "4a 按核定 80 承担" 80 "$(echo "$REQ_BODY" | jqr charge_total)"
assert_eq "4a 退还 20" 20 "$(echo "$REQ_BODY" | jqr refund_to_tenant)"
assert_eq "4a 结算口径 MANUAL_ADJUDICATED" MANUAL_ADJUDICATED "$(echo "$REQ_BODY" | jqr settle_kind)"

########## 场景 4b: 分摊争议 -> 主结算闸门 -> 裁决 -> 恢复结算 ##########
info "场景4b: 分摊争议 MANUAL_REVIEW -> 主押金结算被阻止 -> 裁决闭合 -> 结算"
CID=$(new_lease 5000 1000)
req POST "/api/v1/contracts/$CID/utility-reservation" "$L" '{"reserved_amount":100}'
RID=$(echo "$REQ_BODY" | jqr reservation_id)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/bills" "$L" '{"bill_no":"BILL-5-001","total_amount":90}'
# 租客对分摊有异议
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/dispute" "$T" \
  '{"comment":"搬离后产生的电费不应由我承担"}'
assert_eq "4b 租客打开水电分摊争议" 201 "$REQ_CODE"
DID=$(echo "$REQ_BODY" | jqr dispute_id)
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "4b 预留保持 MANUAL_REVIEW" MANUAL_REVIEW "$RST"
# 最终结算被应用层+触发器阻止
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "4b 争议中最终结算被拒(409)" 409 "$REQ_CODE"
# 主押金结算同样被阻止(应用层 409)
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"SETTLED","cleaning_status":"CLEAN"}'
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "4b 水电预留未结清主结算被拒(409)" 409 "$REQ_CODE"
# DB 级兜底: 绕过应用直接插最终结算也必须回滚
GUARD=$(PSQL "INSERT INTO final_utility_settlements(contract_id,reservation_id,bill_id,
  reserved_amount,charge_total,from_deposit,refund_to_tenant,supplement_amount,settle_kind,created_by)
  SELECT '$CID','$RID',id,100,90,90,10,0,'BILL_ONLY','$LID'
  FROM utility_bills WHERE reservation_id='$RID' AND status='RECEIVED'" 2>&1 || true)
echo "$GUARD" | grep -qE 'CONFLICT_OPEN_DISPUTE|CONFLICT_UTILITY_BAD_STATE' \
  && ok "4b DB 触发器拒绝争议/人工态下的最终结算" || bad "4b DB 触发器未生效: $GUARD"
# 平台裁决(无资金挂项, 仅闭合争议)
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$P" \
  '{"action":"ADJUDICATE","amount":0,"comment":"账单期间覆盖租约内, 但90元中10元为搬离后用量, 核定80"}'
assert_eq "4b 平台裁决闭合争议" 200 "$REQ_CODE"
# 裁决后仍需平台把人工态恢复, 并把核定金额 80 写入账单
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/manual-resolve" "$P" \
  '{"action":"RESUME","settled_amount":80}'
assert_eq "4b 人工复核恢复" 200 "$REQ_CODE"
# 争议未闭合时恢复应被拒(此处已闭合, 反向校验前置)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "4b 恢复后最终结算" 201 "$REQ_CODE"
assert_eq "4b 按裁决核定 80 承担" 80 "$(echo "$REQ_BODY" | jqr charge_total)"
assert_eq "4b 退还 20" 20 "$(echo "$REQ_BODY" | jqr refund_to_tenant)"
assert_eq "4b 唯一最终流水" 1 "$(final_count "$CID")"

########## 场景 4c: 读数无法对应 UNMATCHABLE ##########
info "场景4c: 照片读数与账单无法对应 -> UNMATCHABLE 人工复核 -> 恢复 -> MANUAL_ADJUDICATED"
CID=$(new_lease 5000 1000)
req POST "/api/v1/contracts/$CID/utility-reservation" "$L" '{"reserved_amount":100}'
RID=$(echo "$REQ_BODY" | jqr reservation_id)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/bills" "$L" '{"bill_no":"BILL-6-001","total_amount":95}'
assert_eq "4c 账单登记" 201 "$REQ_CODE"
EV=$(mk_evidence "$CID" unmatch)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/meter-photos" "$T" \
  "{\"photos\":[{\"kind\":\"ELECTRIC\",\"reading\":9999,\"evidence_id\":\"$EV\"}]}"
assert_eq "4c 补传照片" 200 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/review" "$P" \
  '{"decision":"UNMATCHABLE","note":"照片表号与账单表号不一致, 读数无法对应"}'
assert_eq "4c 复核 UNMATCHABLE" 200 "$REQ_CODE"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
MR=$(PSQL "SELECT manual_reason FROM utility_reservations WHERE id='$RID'")
assert_eq "4c 预留 MANUAL_REVIEW" MANUAL_REVIEW "$RST"
assert_eq "4c 人工原因 READING_UNMATCHABLE" READING_UNMATCHABLE "$MR"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "4c 读数无法对应时禁止结算(409)" 409 "$REQ_CODE"
# 平台线下核对后恢复(账单在途, 按账单金额 95)
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/manual-resolve" "$P" \
  '{"action":"RESUME","note":"线下核对表号, 账单95元无误"}'
assert_eq "4c 人工恢复" 200 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/utility-reservation/$RID/finalize" "$L"
assert_eq "4c 恢复后最终结算" 201 "$REQ_CODE"
assert_eq "4c 实账 95 承担" 95 "$(echo "$REQ_BODY" | jqr charge_total)"
assert_eq "4c 退还 5" 5 "$(echo "$REQ_BODY" | jqr refund_to_tenant)"
assert_eq "4c 结算口径 MANUAL_ADJUDICATED" MANUAL_ADJUDICATED "$(echo "$REQ_BODY" | jqr settle_kind)"

########## 通用资金勾稽: 所有已结清预留 escrow 三层金额自洽 ##########
info "收尾: 数据库级资金勾稽"
BAD=$(PSQL "SELECT count(*) FROM escrow_accounts e WHERE e.hard_hold + e.disputed_hold > e.balance")
assert_eq "escrow 约束 hard+disputed<=balance 全部自洽" 0 "$BAD"
DUPF=$(PSQL "SELECT count(*) FROM (
  SELECT contract_id FROM final_utility_settlements GROUP BY contract_id HAVING count(*)>1) x")
assert_eq "每份合同至多一笔水电最终结算" 0 "$DUPF"
DUPB=$(PSQL "SELECT count(*) FROM (
  SELECT contract_id,bill_no FROM utility_bills GROUP BY contract_id,bill_no HAVING count(*)>1) x")
assert_eq "同一账单号在同合同无重复" 0 "$DUPB"
# 台账勾稽: 每笔已结清预留, UTILITY_FINAL_SETTLE 出账金额之和 == 预留金额
RECON=$(PSQL "
  SELECT count(*) FROM utility_reservations r WHERE r.status='SETTLED' AND (
    SELECT COALESCE(sum(amount),0) FROM fund_ledger f
      WHERE f.contract_id=r.contract_id AND f.txn_type='UTILITY_FINAL_SETTLE') <> r.reserved_amount")
assert_eq "最终流水金额之和=预留金额(托管与最终结果一致)" 0 "$RECON"

echo
echo "水电尾款场景结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
