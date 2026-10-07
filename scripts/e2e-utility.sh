#!/usr/bin/env bash
# 退租水电尾款结算端到端验证。
#
# 覆盖:
#  A. 预留原因/预计结算时间展示; 搬离照片与供应商账单一起复核一致 -> 多退;
#     重复账单/重复补传照片幂等(只有一笔 UTILITY_FINAL); 验房不重复计水电; 最终放款。
#  B. 账单读数与照片读数无法对应 -> 预留挂起人工复核, 结算被 DB 闸门阻止; 平台人工定论后放款。
#  C. 双方对费用分摊有争议 -> 预留保持待处理并进入人工复核; 平台裁决后只产生一笔最终流水。
#  D. 实际费用 > 预留 -> 向租客发起补缴; 补缴未到账不能结算; 到账后托管账户与最终结果一致。
#
# 用法(本地用户级实例默认 5433):
#   bash scripts/e2e-utility.sh
#   PGPORT=5432 PGUSER=app PGPASSWORD=app PGDATABASE=deposit bash scripts/e2e-utility.sh
set -uo pipefail
BASE="${BASE:-http://localhost:8080}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=e2e-lib.sh
source "$HERE/e2e-lib.sh"

T="tenant-token"; L="landlord-token"; P="platform-token"

PID=$(PSQL "SELECT id FROM properties WHERE address='建国路 88 号 SOHO 现代城 3-1207' LIMIT 1")
TID=$(PSQL "SELECT id FROM users WHERE phone='13800000001'")

# 证据(搬离当天水表/电表照片)需要 64 位 sha256。
sha() { printf 'moveout-photo-%s' "$1" | sha256sum | cut -d' ' -f1; }

# mk_contract <标签> <入住水读数> <入住电读数> <退租水读数> <退租电读数> [押金]
# 完成: 签约 -> 押金托管 -> 入住交割 -> 退租交房; 导出 CID。
mk_contract() {
  local tag="$1" win="$2" ein="$3" wout="$4" eout="$5" dep="${6:-10000}"
  req POST /api/v1/contracts "$L" "{\"property_id\":\"$PID\",\"tenant_id\":\"$TID\",
    \"start_date\":\"2026-01-01\",\"end_date\":\"2026-12-31\",\"rent_amount\":5000,
    \"deposit_amount\":$dep,\"notice_days\":30,\"inspection_due_days\":7,
    \"lease_no\":\"UT-$tag-$(date +%s)-$RANDOM\"}"
  CID=$(echo "$REQ_BODY" | jqr id)
  req POST "/api/v1/contracts/$CID/deposit" "$T"
  req POST "/api/v1/contracts/$CID/checkin" "$L" "{
    \"water_meter_reading\":$win,\"elec_meter_reading\":$ein,
    \"keys_handover\":\"钥匙2把\",\"cleaning_note\":\"清洁\",
    \"meters\":[{\"kind\":\"WATER\",\"reading\":$win,\"unit_price\":5},
               {\"kind\":\"ELECTRIC\",\"reading\":$ein,\"unit_price\":0.6}]}"
  req POST "/api/v1/contracts/$CID/checkout" "$T" "{
    \"water_meter_reading\":$wout,\"elec_meter_reading\":$eout,
    \"keys_returned\":\"钥匙2把\",
    \"meters\":[{\"kind\":\"WATER\",\"reading\":$wout,\"unit_price\":5},
               {\"kind\":\"ELECTRIC\",\"reading\":$eout,\"unit_price\":0.6}]}"
}

# 上传一张搬离表计照片证据并提交读数。
photo() { # <cid> <kind> <reading> <tag>
  local cid="$1" kind="$2" reading="$3" tag="$4"
  req POST "/api/v1/contracts/$cid/evidence" "$T" \
    "{\"category\":\"UTILITY\",\"sha256\":\"$(sha "$tag")\",\"size_bytes\":1024,
      \"storage_uri\":\"oss://moveout/$tag.jpg\",\"caption\":\"$kind 搬离当天读数 $reading\"}"
  EV_LAST=$(echo "$REQ_BODY" | jqr id)
  req POST "/api/v1/contracts/$cid/utility-reserve/photos" "$T" \
    "{\"kind\":\"$kind\",\"reading\":$reading,\"evidence_id\":\"$EV_LAST\",\"taken_at\":\"2026-12-31T10:00:00Z\"}"
}

final_count() { PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$1' AND txn_type='UTILITY_FINAL'"; }

# 走完验房阶段(无墙面等其它扣款), 使合同进入 DEDUCTION_PROPOSED 以便最终结算。
# 水电已由预留覆盖时 utility_status 传 SETTLED, 验房不会再生成水电欠费主张。
inspect() { # <cid> [utility_status]
  local cid="$1"; local ust="${2:-SETTLED}"
  req POST "/api/v1/contracts/$cid/inspection/start" "$L"
  req POST "/api/v1/contracts/$cid/inspection/submit" "$L" "{
    \"wall_status\":\"GOOD\",\"floor_status\":\"GOOD\",\"appliance_status\":\"GOOD\",
    \"keys_status\":\"RETURNED\",\"utility_status\":\"$ust\",\"cleaning_status\":\"CLEAN\"}"
}

###############################################################################
info "A. 预留展示 + 照片账单复核一致 -> 多退; 幂等; 验房不重复计水电"
mk_contract A 100 2000 112 2300
req POST "/api/v1/contracts/$CID/utility-reserve" "$L" '{"expected_settle_days":30}'
assert_eq "A 创建预留" 201 "$REQ_CODE"
RID=$(echo "$REQ_BODY" | jqr reservation_id)
RAMT=$(echo "$REQ_BODY" | jqr reserve_amount)
# 水 (112-100)*5=60, 电 (2300-2000)*0.6=180 => 240
assert_eq "A 预留金额=240(最近读数+历史用量)" 240 "$RAMT"
UH=$(PSQL "SELECT utility_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "A 押金第四层 utility_hold=240" 240.00 "$UH"
# 预留原因与预计结算时间展示给双方
req GET "/api/v1/contracts/$CID/utility-reserve" "$T"
REASON=$(echo "$REQ_BODY" | python3 -c "import sys,json;print(json.load(sys.stdin).get('reason',''))" 2>/dev/null)
EXP=$(echo "$REQ_BODY" | python3 -c "import sys,json;print(json.load(sys.stdin).get('expected_settle_at',''))" 2>/dev/null)
echo "$REASON" | grep -q "账单尚未生成" && ok "A 展示预留原因" || bad "A 预留原因缺失: $REASON"
[ -n "$EXP" ] && ok "A 展示预计结算时间=$EXP" || bad "A 预计结算时间缺失"
ITEMS=$(PSQL "SELECT count(*) FROM utility_reserve_items WHERE reservation_id='$RID'")
assert_eq "A 预留分品类明细=2" 2 "$ITEMS"

# 验房标记水电未结清, 但因已有预留, 不得重复生成水电欠费主张
req POST "/api/v1/contracts/$CID/inspection/start" "$L"
req POST "/api/v1/contracts/$CID/inspection/submit" "$L" '{
  "wall_status":"GOOD","floor_status":"GOOD","appliance_status":"GOOD",
  "keys_status":"RETURNED","utility_status":"UNPAID","cleaning_status":"CLEAN"}'
UCHARGE=$(PSQL "SELECT count(*) FROM charge_items WHERE contract_id='$CID' AND kind='UTILITY_UNPAID'")
assert_eq "A 验房不重复生成水电欠费主张" 0 "$UCHARGE"
DISP=$(PSQL "SELECT disputed_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "A disputed_hold 未被水电建议占用=0" 0.00 "$DISP"

# 租客上传搬离当天水/电表照片
photo "$CID" WATER 112 A-water >/dev/null
assert_eq "A 上传水表照片" 201 "$REQ_CODE"
photo "$CID" ELECTRIC 2300 A-elec >/dev/null
EV_ELEC=$EV_LAST
assert_eq "A 上传电表照片" 201 "$REQ_CODE"

# 第一张账单到达: 电账单仍缺 -> 账单延迟, 保持待处理, 不结算
req POST "/api/v1/contracts/$CID/utility-reserve/bills" "$L" \
  '{"bill_no":"BW-A-1","kind":"WATER","billed_reading":112,"amount":60,"period_end":"2026-12-31"}'
assert_eq "A 收水费账单" 201 "$REQ_CODE"
BLK=$(echo "$REQ_BODY" | jqr blocker)
assert_eq "A 仅一张账单 -> BILL_DELAY 挂起" BILL_DELAY "$BLK"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "A 预留状态 REVIEW_PENDING" REVIEW_PENDING "$RST"
assert_eq "A 挂起期间无最终流水" 0 "$(final_count "$CID")"

# 电费账单到达, 与照片读数一致, 但费用仅 150(供应商优惠) => 实际 210 < 预留 240, 退 30
req POST "/api/v1/contracts/$CID/utility-reserve/bills" "$L" \
  '{"bill_no":"BE-A-1","kind":"ELECTRIC","billed_reading":2300,"amount":150,"period_end":"2026-12-31"}'
assert_eq "A 收电费账单并自动结算" 201 "$REQ_CODE"
FIN=$(echo "$REQ_BODY" | jqr finalized)
assert_eq "A 复核一致自动 FINALIZED" True "$FIN"
REF=$(echo "$REQ_BODY" | jqr refund_amount)
assert_eq "A 多退=30" 30 "$REF"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "A 预留状态 FINALIZED" FINALIZED "$RST"
assert_eq "A 只有一笔最终押金流水" 1 "$(final_count "$CID")"
UH=$(PSQL "SELECT utility_hold FROM escrow_accounts WHERE contract_id='$CID'")
HARD=$(PSQL "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "A 结算后 utility_hold 清零" 0.00 "$UH"
assert_eq "A 实际费用 60+150=210 进入 hard_hold" 210.00 "$HARD"

# 重复收到同一电费账单: 幂等, reused=true, 不产生第二笔扣款
req POST "/api/v1/contracts/$CID/utility-reserve/bills" "$L" \
  '{"bill_no":"BE-A-1","kind":"ELECTRIC","billed_reading":2300,"amount":150}'
REUSED=$(echo "$REQ_BODY" | jqr reused)
assert_eq "A 重复账单幂等 reused=True" True "$REUSED"
assert_eq "A 重复账单后仍只有一笔最终流水" 1 "$(final_count "$CID")"
# 重复补传同一张照片(同一 evidence): 幂等, 不重复扣款
req POST "/api/v1/contracts/$CID/utility-reserve/photos" "$T" \
  "{\"kind\":\"ELECTRIC\",\"reading\":2300,\"evidence_id\":\"$EV_ELEC\",\"taken_at\":\"2026-12-31T10:00:00Z\"}"
assert_eq "A 重复补传照片幂等" 201 "$REQ_CODE"
assert_eq "A 重复照片后仍只有一笔最终流水" 1 "$(final_count "$CID")"

# 双方确认结算放款(水电 210 付房东, 其余含多退 30 共 9790 退租客)
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "A 生成结算单" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$T"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$L"
assert_eq "A 双方确认放款" 200 "$REQ_CODE"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
REL=$(PSQL "SELECT released_total FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "A 托管账户最终余额清零" 0.00 "$BAL"
assert_eq "A 累计释放=押金 10000" 10000.00 "$REL"

###############################################################################
info "B. 账单读数与照片无法对应 -> 挂起人工复核 + DB 闸门 -> 平台定论"
mk_contract B 100 2000 112 2300
req POST "/api/v1/contracts/$CID/utility-reserve" "$L" '{}'
RID=$(echo "$REQ_BODY" | jqr reservation_id)
photo "$CID" WATER 112 B-water >/dev/null
# 账单读数 130 与搬离照片 112 相差 18(>容差), 不能只按账单扣款 -> READING_MISMATCH
req POST "/api/v1/contracts/$CID/utility-reserve/bills" "$L" \
  '{"bill_no":"BW-B-1","kind":"WATER","billed_reading":130,"amount":150}'
assert_eq "B 收水费账单" 201 "$REQ_CODE"
BLK=$(echo "$REQ_BODY" | jqr blocker)
assert_eq "B 读数无法对应 -> READING_MISMATCH" READING_MISMATCH "$BLK"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "B 预留进入 MANUAL_REVIEW" MANUAL_REVIEW "$RST"
assert_eq "B 挂起期间不扣款(无最终流水)" 0 "$(final_count "$CID")"
UH=$(PSQL "SELECT utility_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "B 预留款保持待处理 utility_hold=240" 240.00 "$UH"

# 走完验房阶段(无其它扣款), 合同进入 DEDUCTION_PROPOSED
inspect "$CID"
# 应用层: 挂起时不能结算
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "B 预留未结算时生成结算被拒(409)" 409 "$REQ_CODE"
# 数据库级闸门: 绕过应用层直接造结算单并 CONFIRMED, 触发器必须回滚
PSQL "INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,paid_to_landlord,status)
      SELECT '$CID',10000,0,10000,0,'PROPOSED'" >/dev/null
GUARD=$(PSQL "UPDATE settlements SET status='CONFIRMED' WHERE contract_id='$CID'" 2>&1 || true)
echo "$GUARD" | grep -q CONFLICT_UTILITY_PENDING \
  && ok "B DB 触发器阻止未最终结算的水电预留放款" || bad "B 触发器未生效: $GUARD"
PSQL "DELETE FROM settlements WHERE contract_id='$CID'" >/dev/null

# 平台人工复核: 照片与账单并列复核后定论水费 55
req POST "/api/v1/contracts/$CID/utility-reserve/manual-finalize" "$P" \
  '{"amounts":{"WATER":55},"note":"账单读数异常, 按搬离照片读数复核酌定55"}'
assert_eq "B 平台人工定论" 200 "$REQ_CODE"
assert_eq "B 人工定论后只有一笔最终流水" 1 "$(final_count "$CID")"
FINAL=$(PSQL "SELECT final_amount FROM utility_reservations WHERE id='$RID'")
assert_eq "B 最终费用=55" 55.00 "$FINAL"
HARD=$(PSQL "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "B hard_hold=55" 55.00 "$HARD"
# 已最终结算后再来新账单不能重复扣款
req POST "/api/v1/contracts/$CID/utility-reserve/bills" "$L" \
  '{"bill_no":"BW-B-2","kind":"WATER","billed_reading":130,"amount":150}'
assert_eq "B 最终结算后新账单被拒(409)" 409 "$REQ_CODE"
assert_eq "B 仍只有一笔最终流水" 1 "$(final_count "$CID")"
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$T"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$L"
assert_eq "B 放款" 200 "$REQ_CODE"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "B 托管余额清零" 0.00 "$BAL"

###############################################################################
info "C. 费用分摊争议 -> 保持待处理人工复核 -> 平台裁决"
mk_contract C 100 2000 112 2300
req POST "/api/v1/contracts/$CID/utility-reserve" "$L" '{}'
RID=$(echo "$REQ_BODY" | jqr reservation_id)
inspect "$CID"
# 租客先就分摊提出争议
req POST "/api/v1/contracts/$CID/utility-reserve/dispute" "$T" \
  '{"kind":"WATER","subject":"退租后空置期水费不应由租客承担","claimed_amount":60}'
assert_eq "C 打开分摊争议" 201 "$REQ_CODE"
DID=$(echo "$REQ_BODY" | jqr dispute_id)
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "C 争议后预留 MANUAL_REVIEW" MANUAL_REVIEW "$RST"
# 账单随后到达, 但存在争议, 不能按账单直接扣款
req POST "/api/v1/contracts/$CID/utility-reserve/bills" "$L" \
  '{"bill_no":"BW-C-1","kind":"WATER","billed_reading":112,"amount":60}'
BLK=$(echo "$REQ_BODY" | jqr blocker)
assert_eq "C 有分摊争议时账单不自动扣款 -> SPLIT_DISPUTE" SPLIT_DISPUTE "$BLK"
assert_eq "C 争议挂起期间无最终流水" 0 "$(final_count "$CID")"
# 平台裁决: 水费租客承担 40
req POST "/api/v1/contracts/$CID/disputes/$DID/actions" "$P" \
  '{"action":"ADJUDICATE","amount":40,"comment":"搬离后3天水费由房东承担, 酌定40"}'
assert_eq "C 平台裁决" 200 "$REQ_CODE"
RST=$(PSQL "SELECT status FROM utility_reservations WHERE id='$RID'")
assert_eq "C 裁决后预留 FINALIZED" FINALIZED "$RST"
assert_eq "C 裁决后只产生一笔最终流水" 1 "$(final_count "$CID")"
FINAL=$(PSQL "SELECT final_amount FROM utility_reservations WHERE id='$RID'")
assert_eq "C 最终费用=40" 40.00 "$FINAL"
# 电品类无账单无争议: 平台裁决按总费用定论时未给电的值 -> 电 0, 预留 240 中 200 退回
HARD=$(PSQL "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "C hard_hold=40" 40.00 "$HARD"
UH=$(PSQL "SELECT utility_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "C utility_hold 清零(余款退回可退余额)" 0.00 "$UH"
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$T"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$L"
assert_eq "C 放款" 200 "$REQ_CODE"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "C 托管余额清零" 0.00 "$BAL"

###############################################################################
info "D. 实际费用 > 预留 -> 租客补缴; 未补缴不能结算; 到账后账户一致"
mk_contract D 100 2000 112 2300 10000
req POST "/api/v1/contracts/$CID/utility-reserve" "$L" '{"kinds":["WATER"]}'
assert_eq "D 仅预留水费" 201 "$REQ_CODE"
RID=$(echo "$REQ_BODY" | jqr reservation_id)
RAMT=$(echo "$REQ_BODY" | jqr reserve_amount)
assert_eq "D 水费预留=60" 60 "$RAMT"
inspect "$CID"
photo "$CID" WATER 112 D-water >/dev/null
# 实际水费 150 > 预留 60 => 不足 90, 向租客发起补缴
req POST "/api/v1/contracts/$CID/utility-reserve/bills" "$L" \
  '{"bill_no":"BW-D-1","kind":"WATER","billed_reading":112,"amount":150}'
assert_eq "D 收账单并结算" 201 "$REQ_CODE"
SHORT=$(echo "$REQ_BODY" | jqr shortage_amount)
assert_eq "D 不足部分=90" 90 "$SHORT"
assert_eq "D 只有一笔最终流水" 1 "$(final_count "$CID")"
HARD=$(PSQL "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "D hard_hold=实际150" 150.00 "$HARD"
# 补缴未到账: 应用层与 DB 闸门双重阻止结算
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "D 补缴未到账禁止结算(409)" 409 "$REQ_CODE"
PSQL "INSERT INTO settlements(contract_id,deposit_amount,deducted_total,returned_to_tenant,paid_to_landlord,status)
      SELECT '$CID',10000,150,9850,150,'PROPOSED'" >/dev/null
GUARD=$(PSQL "UPDATE settlements SET status='CONFIRMED' WHERE contract_id='$CID'" 2>&1 || true)
echo "$GUARD" | grep -q CONFLICT_UTILITY_TOPUP \
  && ok "D DB 触发器阻止补缴未到账放款" || bad "D 触发器未生效: $GUARD"
PSQL "DELETE FROM settlements WHERE contract_id='$CID'" >/dev/null
# 非租客不能补缴(房东代缴在本演示中拒绝)
req POST "/api/v1/contracts/$CID/utility-reserve/topup" "$L"
assert_eq "D 房东补缴被拒(403)" 403 "$REQ_CODE"
# 租客补缴 90
req POST "/api/v1/contracts/$CID/utility-reserve/topup" "$T"
assert_eq "D 租客补缴到账" 200 "$REQ_CODE"
TOP=$(echo "$REQ_BODY" | jqr topped_up)
assert_eq "D 补缴金额=90" 90 "$TOP"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
HARD=$(PSQL "SELECT hard_hold FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "D 补缴后托管余额=10090" 10090.00 "$BAL"
assert_eq "D 补缴后 hard_hold=240(150+90)" 240.00 "$HARD"
# 重复补缴被拒(不能超过 shortage)
req POST "/api/v1/contracts/$CID/utility-reserve/topup" "$T"
assert_eq "D 已补齐后再次补缴被拒(409)" 409 "$REQ_CODE"
# 结算: 240 付房东(含补缴90), 9850 退租客, 托管清零
req POST "/api/v1/contracts/$CID/settlement/propose" "$L"
assert_eq "D 补缴到账后允许结算" 201 "$REQ_CODE"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$T"
req POST "/api/v1/contracts/$CID/settlement/confirm" "$L"
assert_eq "D 双方确认放款" 200 "$REQ_CODE"
BAL=$(PSQL "SELECT balance FROM escrow_accounts WHERE contract_id='$CID'")
REL=$(PSQL "SELECT released_total FROM escrow_accounts WHERE contract_id='$CID'")
assert_eq "D 托管账户最终清零" 0.00 "$BAL"
assert_eq "D 累计释放=押金10000+补缴90=10090" 10090.00 "$REL"
TOPLEDGER=$(PSQL "SELECT count(*) FROM fund_ledger WHERE contract_id='$CID' AND txn_type='UTILITY_TOPUP'")
assert_eq "D 补缴流水=1 笔" 1 "$TOPLEDGER"

echo
echo "水电尾款场景结果: PASS=$PASS FAIL=$FAIL SKIP=$SKIP"
[ "$FAIL" -eq 0 ]
