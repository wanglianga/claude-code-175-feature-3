-- 退租水电尾款结算(最后一期账单未出时的押金预留与最终结算)
--
-- 背景: 租客搬离当天, 水电煤供应商最后一期账单往往尚未生成。平台依据最近读数与历史账单
--       从押金中预留一部分(utility_reservations), 向双方展示预留原因与预计结算时间;
--       账单到达后登记供应商账单(utility_bills, 账单号幂等), 租客可补传搬离当天的水表/电表
--       照片读数(meter_photos), 平台把照片读数与账单逐项复核(meter_reviews)后只生成一笔
--       最终押金流水(final_utility_settlements): 多退(退还租客)少补(utility_supplements 补缴)。
--       账单延迟 / 读数无法对应 / 分摊争议 -> 预留款保持 MANUAL_REVIEW 人工复核, 不得最终结算。
--
-- 资金联动(与 0001 的 escrow 三层金额同一套机制, 同一合同 advisory lock 事务内完成):
--   预留:   available(未占用余额) -> hard_hold                 fund_ledger: UTILITY_RESERVE
--   最终:   整笔预留离开托管(支付供应商/房东 + 退还租客)        fund_ledger: UTILITY_FINAL_SETTLE
--   不足:   不动托管, 形成向租客的补缴应收(utility_supplements)
-- 不变量:
--   1. 一份合同至多一份预留、至多一笔最终结算(UNIQUE 约束数据库级保证)。
--   2. 同一供应商账单号重复上传不产生第二笔扣款((contract_id,bill_no) 唯一)。
--   3. 最终结算金额必须与预留金额勾稽: from_deposit + refund_to_tenant = reserved_amount,
--      且 from_deposit = LEAST(charge_total, reserved_amount)(CHECK 约束)。
--   4. 存在未闭合争议(含水电尾款分摊争议)时, 最终结算插入即被触发器回滚。

-- fund_ledger 新增两类台账流水
ALTER TABLE fund_ledger DROP CONSTRAINT IF EXISTS fund_ledger_txn_type_check;
ALTER TABLE fund_ledger ADD CONSTRAINT fund_ledger_txn_type_check CHECK (txn_type IN
    ('DEPOSIT_IN','HOLD_PROPOSE','HOLD_CONFIRM','HOLD_RELEASE',
     'DEDUCT','RELEASE_TO_TENANT','RELEASE_TO_LANDLORD','ADJUST',
     'UTILITY_RESERVE','UTILITY_FINAL_SETTLE'));

-- 争议原因补充: 水电尾款(账单延迟/读数不符/费用分摊)
ALTER TABLE disputes DROP CONSTRAINT IF EXISTS disputes_reason_check;
ALTER TABLE disputes ADD CONSTRAINT disputes_reason_check CHECK (reason IN
    ('WALL_DAMAGE_VS_WEAR','FLOOR_DAMAGE_VS_WEAR','APPLIANCE_RESPONSIBILITY',
     'UTILITY_ARREARS','KEY_LOSS','INSPECTION_DELAY','EARLY_TERMINATION',
     'CLEANING_FEE','UTILITY_FINAL_SETTLEMENT','OTHER'));

-- ============ 水电尾款预留 ============
-- status:
--   RESERVED       已预留, 等待最后一期供应商账单
--   BILL_RECEIVED  账单已到, 尚无租客照片或不需照片复核, 可最终结算
--   REVIEW_PENDING 租客已补传搬离当天读数照片, 等待平台把照片与账单放一起复核
--   MANUAL_REVIEW  账单延迟/读数无法对应/分摊有争议 -> 预留款保持待处理, 人工复核
--   SETTLED        已最终结算(且仅有一笔最终押金流水)
CREATE TABLE utility_reservations (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL UNIQUE REFERENCES contracts(id),
    status              text NOT NULL DEFAULT 'RESERVED'
        CHECK (status IN ('RESERVED','BILL_RECEIVED','REVIEW_PENDING','MANUAL_REVIEW','SETTLED')),
    reserved_amount     cny_amount NOT NULL CHECK (reserved_amount > 0),
    -- 分项估算依据: [{kind, last_reading, est_usage, unit_price, est_amount, basis}]
    estimate_breakdown  jsonb NOT NULL DEFAULT '[]',
    -- 预留原因(展示给租客与房东): 最后一期账单未出 + 估算口径
    reason              text NOT NULL,
    expected_settle_at  timestamptz NOT NULL,            -- 预计结算时间(展示给双方)
    bill_due_at         timestamptz,                     -- 供应商账单预计出账日
    bill_overdue        boolean NOT NULL DEFAULT false,  -- 账单延迟标记(平台巡检置位)
    -- 进入 MANUAL_REVIEW 的具体原因: BILL_DELAYED 账单延迟 / READING_UNMATCHABLE 读数无法对应 /
    -- ALLOCATION_DISPUTE 双方对费用分摊有争议; 状态恢复时按此原因判定能否自动回到可结算
    manual_reason       text CHECK (manual_reason IN
                            ('BILL_DELAYED','READING_UNMATCHABLE','ALLOCATION_DISPUTE')),
    note                text,
    created_by          uuid NOT NULL REFERENCES users(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    status_updated_at   timestamptz NOT NULL DEFAULT now(),
    finalized_at        timestamptz
);
CREATE INDEX idx_utility_reservations_status ON utility_reservations(status, expected_settle_at);

-- ============ 供应商最后一期账单 ============
-- bill_no 在同一合同内唯一: 重复收到同一账单只返回原记录, 绝不重复扣款。
CREATE TABLE utility_bills (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    reservation_id  uuid NOT NULL REFERENCES utility_reservations(id),
    bill_no         text NOT NULL,
    supplier        text NOT NULL DEFAULT '水电煤供应商',
    bill_period_start date,
    bill_period_end   date,
    -- 分项: [{kind, start_reading, end_reading, usage, unit_price, amount}]
    line_items      jsonb NOT NULL DEFAULT '[]',
    total_amount    cny_amount NOT NULL CHECK (total_amount >= 0),
    -- 平台复核核定金额: NULL=尚未复核(按 total_amount 展示); 复核后可能因照片读数改数。
    -- 最终结算以 COALESCE(settled_amount,total_amount) 为准, 不直接改账单原值, 保留审计痕迹。
    settled_amount  cny_amount,
    evidence_ids    uuid[] NOT NULL DEFAULT '{}',
    note            text,
    -- RECEIVED 已登记待复核/结算; ADOPTED 已被最终结算采用; SUPERSEDED 被复核后的口径替代; REJECTED 复核不采信
    status          text NOT NULL DEFAULT 'RECEIVED'
        CHECK (status IN ('RECEIVED','ADOPTED','SUPERSEDED','REJECTED')),
    uploaded_by     uuid NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, bill_no)
);
CREATE INDEX idx_utility_bills_reservation ON utility_bills(reservation_id);
-- 同一预留至多一份生效中的账单(最终结算后旧单 ADOPTED, 允许再传新单但只能进人工复核)
CREATE UNIQUE INDEX uq_utility_bill_active ON utility_bills(reservation_id)
    WHERE status = 'RECEIVED';

-- ============ 租客补传的搬离当天水表/电表照片读数 ============
-- 照片本体走 evidence(category=UTILITY), 这里只存读数与主张; 复核时与供应商账单并排展示。
-- 同一类表同一预留只保留一条"有效"读数, 重新补传会把旧记录标记 superseded(可审计, 不丢证据)。
CREATE TABLE meter_photos (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    reservation_id  uuid NOT NULL REFERENCES utility_reservations(id),
    kind            text NOT NULL CHECK (kind IN ('WATER','ELECTRIC','GAS')),
    reading         numeric(12,2) NOT NULL CHECK (reading >= 0),
    evidence_id     uuid NOT NULL REFERENCES evidence(id),
    taken_at        timestamptz NOT NULL,                 -- 照片拍摄时间(主张为搬离当天)
    note            text,
    superseded      boolean NOT NULL DEFAULT false,
    uploaded_by     uuid NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_meter_photo_active ON meter_photos(reservation_id, kind)
    WHERE superseded = false;
CREATE INDEX idx_meter_photos_reservation ON meter_photos(reservation_id);

-- ============ 照片读数 vs 供应商账单复核记录 ============
-- decision:
--   MATCH          照片读数与账单起止读数对得上, 按账单金额结算
--   ADOPT_PHOTO    账单读数有误/读不到表, 采用照片读数 × 单价重新核算
--   UNMATCHABLE    读数无法对应(表号/期间不一致等), 转人工复核
CREATE TABLE meter_reviews (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    reservation_id  uuid NOT NULL REFERENCES utility_reservations(id),
    bill_id         uuid REFERENCES utility_bills(id),
    decision        text NOT NULL CHECK (decision IN ('MATCH','ADOPT_PHOTO','UNMATCHABLE')),
    -- 逐项复核: [{kind, bill_end_reading, photo_reading, adopted_reading, unit_price, amount, result}]
    line_results    jsonb NOT NULL DEFAULT '[]',
    note            text,
    reviewed_by     uuid NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_meter_reviews_reservation ON meter_reviews(reservation_id);

-- ============ 水电尾款最终结算(一份合同仅一笔) ============
-- 这是水电尾款唯一的最终押金流水: 重复账单/重复补传照片都不会再产生第二笔。
-- 金额勾稽(数据库 CHECK):
--   from_deposit   = LEAST(charge_total, reserved_amount)  托管内承担(打给供应商/房东)
--   refund         = reserved_amount - from_deposit        多出的预留退还租客
--   supplement     = GREATEST(charge_total-reserved_amount,0) 不足部分向租客补缴
--   from_deposit + refund = reserved_amount                托管账户与最终结果一致
CREATE TABLE final_utility_settlements (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL UNIQUE REFERENCES contracts(id),
    reservation_id      uuid NOT NULL UNIQUE REFERENCES utility_reservations(id),
    bill_id             uuid REFERENCES utility_bills(id),
    meter_review_id     uuid REFERENCES meter_reviews(id),
    reserved_amount     cny_amount NOT NULL,
    charge_total        cny_amount NOT NULL,
    from_deposit        cny_amount NOT NULL,
    refund_to_tenant    cny_amount NOT NULL,
    supplement_amount   cny_amount NOT NULL,
    charge_breakdown    jsonb NOT NULL DEFAULT '[]',
    -- REVIEW_CROSS_CHECK 照片与账单复核一致; BILL_ONLY 无照片按实账; MANUAL_ADJUDICATED 人工复核定论
    settle_kind         text NOT NULL CHECK (settle_kind IN
                            ('REVIEW_CROSS_CHECK','BILL_ONLY','MANUAL_ADJUDICATED')),
    note                text,
    created_by          uuid NOT NULL REFERENCES users(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    -- 金额勾稽数据库级保证: 托管与最终结果必须一致
    CHECK (from_deposit = LEAST(charge_total, reserved_amount)),
    CHECK (refund_to_tenant = reserved_amount - from_deposit),
    CHECK (supplement_amount = GREATEST(charge_total - reserved_amount, 0))
);

-- 补缴应收(最终结算时 charge_total 超出预留的部分; 不入托管, 租客另行支付)
CREATE TABLE utility_supplements (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL REFERENCES contracts(id),
    final_settlement_id uuid NOT NULL UNIQUE REFERENCES final_utility_settlements(id),
    amount              cny_amount NOT NULL CHECK (amount > 0),
    status              text NOT NULL DEFAULT 'PENDING'
        CHECK (status IN ('PENDING','PAID','CANCELLED')),
    note                text,
    paid_by             uuid REFERENCES users(id),
    paid_at             timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_utility_supplements_status ON utility_supplements(status);

-- ============ 触发器: 最终结算前置闸门 ============
-- (1) 不允许在 MANUAL_REVIEW / RESERVED(账单未到) 状态下落最终结算;
-- (2) 合同存在任何未闭合争议(含 UTILITY_FINAL_SETTLEMENT 分摊争议)时回滚;
-- (3) 金额勾稽关系由列 CHECK 保证, 这里再兜底校验预留金额。
CREATE OR REPLACE FUNCTION trg_final_utility_guard() RETURNS trigger AS $$
DECLARE
    rst text;
    n   int;
BEGIN
    SELECT status INTO rst FROM utility_reservations WHERE id = NEW.reservation_id;
    -- 仅 BILL_RECEIVED 可最终结算: RESERVED 账单未到; REVIEW_PENDING 照片尚未与账单复核;
    -- MANUAL_REVIEW 账单延迟/读数无法对应/分摊争议, 预留款必须保持待处理。
    IF rst <> 'BILL_RECEIVED' THEN
        RAISE EXCEPTION 'CONFLICT_UTILITY_BAD_STATE: reservation status=% cannot finalize', rst
            USING ERRCODE = 'check_violation';
    END IF;
    PERFORM assert_no_open_dispute(NEW.contract_id);
    IF (SELECT reserved_amount FROM utility_reservations WHERE id = NEW.reservation_id)
       IS DISTINCT FROM NEW.reserved_amount THEN
        RAISE EXCEPTION 'CONFLICT_UTILITY_RESERVE_MISMATCH: reserved_amount changed'
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.from_deposit + NEW.refund_to_tenant <> NEW.reserved_amount THEN
        RAISE EXCEPTION 'CONFLICT_UTILITY_LEDGER_MISMATCH: from_deposit + refund <> reserved'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER final_utility_guard
BEFORE INSERT ON final_utility_settlements
FOR EACH ROW EXECUTE FUNCTION trg_final_utility_guard();

-- ============ 增强主结算守卫: 水电尾款预留未结清时, 主押金结算不得确认 ============
-- 预留款是托管押金的一部分(UTILITY_RESERVE 后计入 hard_hold), 必须先完成水电尾款的
-- "一笔最终押金流水"(多退少补), 主结算才能确认, 否则托管余额无法与最终结果勾稽。
CREATE OR REPLACE FUNCTION trg_settlement_guard() RETURNS trigger AS $$
DECLARE
    n int;
BEGIN
    -- 只守卫 PROPOSED -> CONFIRMED 这一步; EXECUTED 是同事务内 CONFIRMED 之后的放款动作。
    IF NEW.status = 'CONFIRMED' AND OLD.status = 'PROPOSED' THEN
        PERFORM assert_no_open_dispute(NEW.contract_id);
        -- 结算扣款金额必须等于: 当前定论扣款预留 + 已通过水电最终流水支付出托管的部分
        IF (SELECT hard_hold FROM escrow_accounts WHERE contract_id = NEW.contract_id)
             + COALESCE((SELECT sum(from_deposit) FROM final_utility_settlements
                          WHERE contract_id = NEW.contract_id), 0)
           IS DISTINCT FROM NEW.deducted_total THEN
            RAISE EXCEPTION 'CONFLICT_HOLD_MISMATCH: deducted_total % != hard_hold + utility_paid',
                NEW.deducted_total USING ERRCODE = 'check_violation';
        END IF;
        -- 水电尾款预留未最终结算时, 主押金结算不得确认
        SELECT count(*) INTO n FROM utility_reservations
          WHERE contract_id = NEW.contract_id AND status <> 'SETTLED';
        IF n > 0 THEN
            RAISE EXCEPTION 'CONFLICT_UTILITY_PENDING: contract=% has % unresolved utility reservation(s)',
                NEW.contract_id, n USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
