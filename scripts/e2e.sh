#!/usr/bin/env bash
# 端到端验证: 覆盖租赁押金托管全生命周期与墙面损坏争议场景。
# 用法: BASE=http://localhost:8080 PGPORT=5432 bash scripts/e2e.sh
set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=e2e-lib.sh
source "$HERE/e2e-lib.sh"

T="tenant-token"; L="landlord-token"; A="agent-token"; P="platform-token"

info "0. 健康检查与 Swagger"
curl -sf "$BASE/healthz" >/dev/null && ok "GET /healthz 200" || bad "healthz"
SC=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/swagger/")
assert_eq "GET /swagger/" 200 "$SC"
SC=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/openapi.yaml")
assert_eq "GET /openapi.yaml" 200 "$SC"
# 未认证必须 401
req GET /api/v1/contracts ""
assert_eq "无 token 访问受保护接口" 401 "$REQ_CODE"

info "1. 签约 + 押金托管"
# 用种子房源
PID=$(PSQL "SELECT id FROM properties WHERE address='建国路 88 号 SOHO 现代城 3-1207' LIMIT 1")
TID=$(PSQL "SELECT id FROM users WHERE phone='13800000001'")
req POST /api/v1/contracts "$L" "{\"property_id\":\"$PID\",\"tenant_id\":\"$TID\",
  \"start_date\":\"2026-01-01\",\"end_date\":\"2026-12-31\",\"rent_amount\":5000,
  \"deposit_amount\":10000,\"wear_clause\":\"正常居住损耗不由租客承担\",\"notice_days\":30,
  \"inspection_due_days\":7}"
assert_eq "房东签约" 201 "$REQ_CODE"
CID=$(echo "$REQ_BODY" | jqr id)
[ -n "$CID" ] && ok "合同 id=$CID" || bad "合同 id 为空"

# 房东不能自己充押金
req POST "/api/v1/contracts/$CID/deposit" "$L"
assert_eq "房东充押金被拒(403)" 403 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/deposit" "$T"
assert_eq "租客充押金入托管" 200 "$REQ_CODE"

BAL=$(PSQL \
  "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "托管余额=10000" 10000.00 "$BAL"
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "合同进入 LIVING" LIVING "$ST"

# 跨租户隔离: 另一名租客不能看本租赁档案
req POST /api/v1/users "$P" '{"phone":"13900000099","display_name":"其他租客","role":"TENANT"}'
T2=$(curl -s -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"phone":"13900000099"}' | jqr token)
req GET "/api/v1/contracts/$CID/timeline" "$T2"
assert_eq "非租赁参与方访问档案被拒(403)" 403 "$REQ_CODE"

info "2. 入住交割(照片/清单/水电读数)"
req POST "/api/v1/contracts/$CID/checkin" "$L" '{
  "water_meter_reading":100,"elec_meter_reading":2000,"gas_meter_reading":50,
  "keys_handover":"入户钥匙2把+门禁卡2张","cleaning_note":"全屋深度清洁",
  "inventory":[
    {"name":"主卧墙面","category":"WALL","checkin_state":"GOOD"},
    {"name":"客厅地板","category":"FLOOR","checkin_state":"GOOD"},
    {"name":"海尔冰箱","category":"APPLIANCE","brand_model":"BCD-470","checkin_state":"GOOD"}
  ],
  "meters":[
    {"kind":"WATER","reading":100,"unit_price":5},
    {"kind":"ELECTRIC","reading":2000,"unit_price":0.6},
    {"kind":"GAS","reading":50,"unit_price":2.5}
  ]}'
assert_eq "入住交割" 201 "$REQ_CODE"

info "3. 入住期间维修: 租客报修墙面, 房东判定租客责任"
req POST "/api/v1/contracts/$CID/repairs" "$T" '{"title":"主卧墙面破洞","description":"搬家具撞破",
  "category":"WALL"}'
assert_eq "租客报修" 201 "$REQ_CODE"
RID=$(echo "$REQ_BODY" | jqr repair_id)
req POST "/api/v1/contracts/$CID/repairs/$RID/decision" "$L" \
  '{"status":"TENANT_RESPONSIBLE","cost_amount":800,"tenant_share":800,"decision_note":"搬家磕碰,租客责任"}'
assert_eq "房东定责租客" 200 "$REQ_CODE"

info "4. 租客退租交房(含水电读数)"
req POST "/api/v1/contracts/$CID/checkout" "$T" '{
  "water_meter_reading":112,"elec_meter_reading":2300,"gas_meter_reading":60,
  "keys_returned":"入户钥匙2把+门禁卡2张","cleaning_note":"已自行打扫",
  "meters":[
    {"kind":"WATER","reading":112,"unit_price":5},
    {"kind":"ELECTRIC","reading":2300,"unit_price":0.6},
    {"kind":"GAS","reading":60,"unit_price":2.5}
  ]}'
assert_eq "退租交房" 201 "$REQ_CODE"
UTIL=$(PSQL \
  "SELECT sum(billed_amount) FROM utility_readings WHERE contract_id='$CID' AND phase='CHECKOUT'")
# 水(112-100)*5=60, 电(2300-2000)*0.6=180, 气(60-50)*2.5=25 => 265
assert_eq "水电煤区间应结=265.00" 265.00 "$UTIL"

info "5. 房东发起验房并提交结论"
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
assert_eq "发起验房" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"DAMAGED","floor_status":"NORMAL_WEAR","appliance_status":"NORMAL_WEAR",
  "keys_status":"RETURNED","utility_status":"UNPAID","cleaning_status":"CLEAN",
  "wall_repair_fee":800,"summary":"墙面破洞1处, 地板自然磨损, 家电正常老化"}'
assert_eq "提交验房结论" 201 "$REQ_CODE"
TOTAL=$(echo "$REQ_BODY" | jqr deductible_total)
# 墙面 800(维修记录租客责任) + 水电欠费 265 = 1065; 地板/家电自然磨损 0 元
assert_eq "建议扣款合计=1065.00" 1065 "$TOTAL"
DISP=$(PSQL \
  "SELECT disputed_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "建议金额进入 disputed_hold 软冻结" 1065.00 "$DISP"
HARD=$(PSQL \
  "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "定论预留 hard_hold=0(租客尚未确认)" 0.00 "$HARD"

FINDING=$(PSQL \
  "SELECT id FROM findings WHERE contract_id='$CID' AND code='WALL_DAMAGE' LIMIT 1")
CHARGE=$(PSQL \
  "SELECT id FROM charge_items WHERE contract_id='$CID' AND kind='UTILITY_UNPAID' LIMIT 1")

info "6. 争议未闭合时, 结算/放款必须被阻止"
req POST "/api/v1/contracts/$CID/items/finding/$FINDING/contest" "$T" \
  '{"reason":"WALL_DAMAGE_VS_WEAR","subject":"墙面是自然老化不是撞破","comment":"入住一年正常开裂"}'
assert_eq "租客对墙面扣款申诉" 201 "$REQ_CODE"
DISPUTE=$(echo "$REQ_BODY" | jqr dispute_id)
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "合同进入 DISPUTING" DISPUTING "$ST"
# 水电欠费项租客接受
req POST "/api/v1/contracts/$CID/items/charge/$CHARGE/accept" "$T"
assert_eq "租客接受水电欠费扣款" 200 "$REQ_CODE"
HARD=$(PSQL \
  "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "已确认项转 hard_hold=265" 265.00 "$HARD"
# 试图在争议打开时直接结算 -> 业务层: 仍有 disputed 项, 409
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "争议未闭合禁止生成结算(409)" 409 "$REQ_CODE"

# 数据库级闸门: 即使绕过应用层直接把结算单置为 CONFIRMED, 触发器也必须回滚。
PSQL "INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,paid_to_landlord,status)
      SELECT '$CID',10000,hard_hold,balance-hard_hold,hard_hold,'PROPOSED'
      FROM escrow_accounts WHERE contract_id='$CID'" >/dev/null
GUARD=$(PSQL "UPDATE settlements SET status='CONFIRMED' WHERE contract_id='$CID'" 2>&1 || true)
echo "$GUARD" | grep -q CONFLICT_OPEN_DISPUTE \
  && ok "DB 触发器拒绝未闭合争议下 CONFIRMED" || bad "DB 触发器未生效: $GUARD"
PSQL "DELETE FROM settlements WHERE contract_id='$CID'" >/dev/null

info "7. 平台裁决: 墙面损失支持房东 500(原主张800)"
req POST "/api/v1/contracts/$CID/disputes/$DISPUTE/actions" "$P" \
  '{"action":"ADJUDICATE","amount":500,"comment":"鉴定为外力损伤但报价偏高,酌定500"}'
assert_eq "平台裁决" 200 "$REQ_CODE"
HARD=$(PSQL \
  "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
DISP=$(PSQL \
  "SELECT disputed_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "裁决后 hard_hold=265+500=765" 765.00 "$HARD"
assert_eq "裁决后 disputed_hold=0" 0.00 "$DISP"
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "争议闭合回到 DEDUCTION_PROPOSED" DEDUCTION_PROPOSED "$ST"

info "8. 结算与双方确认放款"
req POST "/api/v1/contracts/$CID/settlement/propose" "$L" '{"note":"墙面500+水电265"}'
assert_eq "生成结算单" 201 "$REQ_CODE"
SID=$(echo "$REQ_BODY" | jqr settlement_id)
req POST "/api/v1/contracts/$CID/settlement/confirm" "$T"
assert_eq "租客确认结算" 200 "$REQ_CODE"
# 仅一方确认时押金不能释放
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "单方确认后合同仍未 RELEASED" DEDUCTION_PROPOSED "$ST"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$L"
assert_eq "房东确认结算并放款" 200 "$REQ_CODE"
ST=$(PSQL "SELECT status FROM contracts WHERE id='$CID'")
assert_eq "合同 RELEASED" RELEASED "$ST"
DEDST=$(PSQL "SELECT status FROM deductions WHERE contract_id='$CID'")
assert_eq "扣款单 APPROVED" APPROVED "$DEDST"
BAL=$(PSQL \
  "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
REL=$(PSQL \
  "SELECT released_total FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "托管余额清零" 0.00 "$BAL"
assert_eq "累计释放=10000" 10000.00 "$REL"

info "9. Kafka 事件验证"
sleep 2
PUB=$(PSQL \
  "SELECT count(*) FROM lease_events WHERE contract_id='$CID' AND published=true")
TOT=$(PSQL \
  "SELECT count(*) FROM lease_events WHERE contract_id='$CID'")
KBROKER_HOSTPORT="${KAFKA_BROKERS:-localhost:9092}"
KBROKER_HOST="${KBROKER_HOSTPORT%%:*}"; KBROKER_PORT="${KBROKER_HOSTPORT##*:}"
if (exec 3<>"/dev/tcp/${KBROKER_HOST}/${KBROKER_PORT}") 2>/dev/null; then
  assert_eq "全部 $TOT 个领域事件已投递 Kafka" "$TOT" "$PUB"
else
  skip "Kafka broker $KBROKER_HOSTPORT 不可达, 跳过 published 断言(outbox 事件仍全部落表: $TOT 条)"
fi
KBIN=$(kafka_consumer_bin)
if [ -z "$KBIN" ]; then
  skip "未找到 kafka-console-consumer.sh, 跳过 Kafka 消费侧断言(outbox published 已校验)"
else
  KCOUNT=$("$KBIN" \
    --bootstrap-server "${KAFKA_BROKERS:-localhost:9092}" --topic "${KAFKA_TOPIC:-lease.events}" \
    --from-beginning --timeout-ms 8000 2>/dev/null | grep -c "$CID" || true)
  [ "$KCOUNT" -ge "$TOT" ] && ok "Kafka 消费到本租赁事件 $KCOUNT 条" || bad "Kafka 事件数 $KCOUNT < $TOT"
fi

echo
echo "主场景结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
