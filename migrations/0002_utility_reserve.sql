-- 退租水电尾款结算: 租客搬离时最后一期水电账单尚未生成, 平台先从托管押金中预留一笔
-- "水电尾款预留"(utility_hold, 第四层资金), 向双方展示预留原因与预计结算时间;
-- 供应商账单到达 + 租客搬离当天表计照片一起复核后, 多退少补, 只产生一笔最终押金流水。
--
-- 与既有三层金额的关系:
--   balance          托管总余额(不变)
--   hard_hold        已定论扣款
--   disputed_hold    争议/待确认扣款
--   utility_hold     水电尾款预留(独立软冻结, 账单未结算前不得释放)
--   约束: hard_hold + disputed_hold + utility_hold <= balance

-- ============ 1. 托管账户增加第四层金额 ============
ALTER TABLE escrow_accounts ADD COLUMN utility_hold cny_amount NOT NULL DEFAULT 0;
ALTER TABLE escrow_accounts DROP CONSTRAINT IF EXISTS escrow_accounts_check;
ALTER TABLE escrow_accounts ADD CONSTRAINT escrow_accounts_holds_check
    CHECK (hard_hold + disputed_hold + utility_hold <= balance);

-- ============ 2. 资金台账增加水电预留相关类型 ============
ALTER TABLE fund_ledger DROP CONSTRAINT IF EXISTS fund_ledger_txn_type_check;
ALTER TABLE fund_ledger ADD CONSTRAINT fund_ledger_txn_type_check CHECK (txn_type IN
    ('DEPOSIT_IN','HOLD_PROPOSE','HOLD_CONFIRM','HOLD_RELEASE',
     'DEDUCT','RELEASE_TO_TENANT','RELEASE_TO_LANDLORD','ADJUST',
     -- 水电尾款: 预留 / 唯一一笔最终结算 / 租客补缴
     'UTILITY_RESERVE','UTILITY_FINAL','UTILITY_TOPUP'));

ALTER TABLE fund_ledger ADD COLUMN utility_after cny_amount NOT NULL DEFAULT 0;

-- 关键幂等闸门: 一条租赁的水电尾款最终结算只能有一笔流水,
-- 重复收到账单/重复复核/补传照片都不可能产生第二笔扣款。
CREATE UNIQUE INDEX uq_fund_ledger_one_utility_final
    ON fund_ledger(contract_id) WHERE txn_type = 'UTILITY_FINAL';

-- ============ 3. 水电尾款预留主单(一条租赁至多一笔) ============
CREATE TABLE utility_reservations (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id         uuid NOT NULL UNIQUE REFERENCES contracts(id),
    status              text NOT NULL DEFAULT 'RESERVED'
        CHECK (status IN ('RESERVED','REVIEW_PENDING','MANUAL_REVIEW','FINALIZED')),
    -- 预留金额(创建时从押金中圈定)与预留依据
    reserve_amount      cny_amount NOT NULL,
    reason              text NOT NULL,
    basis_snapshot      jsonb NOT NULL DEFAULT '{}',  -- 最近读数/历史用量/单价快照
    expected_settle_at  date NOT NULL,                -- 预计结算时间(展示给双方)
    -- 复核与最终结算结果
    billed_total        cny_amount NOT NULL DEFAULT 0, -- 供应商账单合计
    photo_total         cny_amount NOT NULL DEFAULT 0, -- 搬离照片读数推算合计
    final_amount        cny_amount NOT NULL DEFAULT 0, -- 复核定论的实际水电费用
    final_decision      text CHECK (final_decision IN ('BILL','PHOTO','MANUAL')),
    review_note         text,
    -- 多退少补
    refund_amount       cny_amount NOT NULL DEFAULT 0, -- 预留多于实际, 已释放回可退余额
    shortage_amount     cny_amount NOT NULL DEFAULT 0, -- 实际多于预留, 向租客发起的补缴
    topup_received      cny_amount NOT NULL DEFAULT 0, -- 租客已补缴到账金额
    finalized_by        uuid REFERENCES users(id),
    created_by          uuid NOT NULL REFERENCES users(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    reviewed_at         timestamptz,
    finalized_at        timestamptz,
    CHECK (refund_amount = 0 OR shortage_amount = 0)   -- 同一笔结算不可能既退又补
);

-- 分币种(水/电/燃气)的预留明细: 估算依据 + 账单读数 + 搬离照片读数 + 复核结论
CREATE TABLE utility_reserve_items (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    reservation_id      uuid NOT NULL REFERENCES utility_reservations(id) ON DELETE CASCADE,
    kind                text NOT NULL CHECK (kind IN ('WATER','ELECTRIC','GAS')),
    -- 估算依据: 最近读数 / 历史月均用量 / 单价 / 估算费用
    last_reading        numeric(12,2) NOT NULL,
    last_reading_at     timestamptz,
    history_avg_usage   numeric(12,2) NOT NULL DEFAULT 0,
    unit_price          numeric(10,4) NOT NULL DEFAULT 0,
    est_amount          cny_amount NOT NULL,
    -- 供应商账单(到达后回填)
    billed_reading      numeric(12,2),
    billed_amount       cny_amount,
    -- 租客搬离当天表计照片(读数由平台人工/程序识别后回填)
    photo_reading       numeric(12,2),
    photo_amount        cny_amount,
    -- 复核定论
    final_reading       numeric(12,2),
    final_amount        cny_amount NOT NULL DEFAULT 0,
    review_flag         text NOT NULL DEFAULT 'OK'
        CHECK (review_flag IN ('OK','BILL_DELAY','READING_MISMATCH','SPLIT_DISPUTE')),
    UNIQUE (reservation_id, kind)
);
CREATE INDEX idx_utility_items_reservation ON utility_reserve_items(reservation_id);

-- ============ 4. 供应商水电账单 ============
-- 同一供应商同一账期单号只允许登记一次: 重复收到同一账单不会触发第二次扣款。
CREATE TABLE utility_bills (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    reservation_id  uuid NOT NULL REFERENCES utility_reservations(id),
    provider        text NOT NULL DEFAULT 'SUPPLIER',
    bill_no         text NOT NULL,
    kind            text CHECK (kind IN ('WATER','ELECTRIC','GAS')), -- NULL=账单含多个品类
    period_start    date,
    period_end      date,
    billed_reading  numeric(12,2),
    amount          cny_amount NOT NULL,
    invoice_evidence_id uuid REFERENCES evidence(id),
    received_by     uuid NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (contract_id, provider, bill_no)
);
CREATE INDEX idx_utility_bills_reservation ON utility_bills(reservation_id);

-- ============ 5. 租客搬离当天水表/电表照片 ============
-- 每个品类在最终结算前只保留一条(补传走覆盖), FINALIZED 后禁止补传/改读数。
CREATE TABLE utility_moveout_photos (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    contract_id     uuid NOT NULL REFERENCES contracts(id),
    reservation_id  uuid NOT NULL REFERENCES utility_reservations(id),
    kind            text NOT NULL CHECK (kind IN ('WATER','ELECTRIC','GAS')),
    reading         numeric(12,2) NOT NULL,        -- 照片表计读数
    reading_from    text NOT NULL DEFAULT 'TENANT_CLAIM'
        CHECK (reading_from IN ('TENANT_CLAIM','PLATFORM_VERIFIED')),
    taken_at        timestamptz NOT NULL,
    evidence_id     uuid NOT NULL REFERENCES evidence(id),
    note            text,
    uploaded_by     uuid NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (reservation_id, kind)
);
CREATE INDEX idx_moveout_photos_contract ON utility_moveout_photos(contract_id);

-- ============ 6. 争议表关联水电预留, 并增加争议原因 ============
ALTER TABLE disputes ADD COLUMN utility_reserve_id uuid REFERENCES utility_reservations(id);
-- 重建 reason CHECK(Postgres 不支持直接给枚举式 CHECK 加值)
ALTER TABLE disputes DROP CONSTRAINT IF EXISTS disputes_reason_check;
ALTER TABLE disputes ADD CONSTRAINT disputes_reason_check CHECK (reason IN
    ('WALL_DAMAGE_VS_WEAR','FLOOR_DAMAGE_VS_WEAR','APPLIANCE_RESPONSIBILITY',
     'UTILITY_ARREARS','KEY_LOSS','INSPECTION_DELAY','EARLY_TERMINATION',
     'CLEANING_FEE','UTILITY_FINAL_SETTLEMENT','OTHER'));
CREATE INDEX idx_disputes_reserve ON disputes(utility_reserve_id)
    WHERE utility_reserve_id IS NOT NULL;
-- 同一笔水电预留同时只允许一个未闭合的分摊/读数争议。
CREATE UNIQUE INDEX uq_dispute_open_reserve ON disputes(utility_reserve_id)
    WHERE utility_reserve_id IS NOT NULL
      AND status IN ('OPEN','EVIDENCE_PENDING','UNDER_REVIEW');

-- ============ 7. 结算单记录水电预留的多退少补 ============
ALTER TABLE settlements ADD COLUMN utility_reserved cny_amount NOT NULL DEFAULT 0;
ALTER TABLE settlements ADD COLUMN utility_topup    cny_amount NOT NULL DEFAULT 0;

DO $$
DECLARE c text;
BEGIN
    FOR c IN
        SELECT conname FROM pg_constraint
        WHERE conrelid = 'settlements'::regclass AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%deducted_total%'
    LOOP
        EXECUTE format('ALTER TABLE settlements DROP CONSTRAINT %I', c);
    END LOOP;
END $$;

ALTER TABLE settlements ADD CONSTRAINT settlements_money_check CHECK (
    paid_to_landlord = deducted_total
    AND deducted_total + returned_to_tenant = deposit_amount + utility_topup);

-- ============ 8. 数据库级闸门: 水电预留未最终结算/补缴未到账, 结算不得确认 ============
CREATE OR REPLACE FUNCTION assert_utility_reserve_final(p_contract uuid) RETURNS void AS $$
DECLARE
    r utility_reservations%ROWTYPE;
BEGIN
    SELECT * INTO r FROM utility_reservations WHERE contract_id = p_contract;
    IF NOT FOUND THEN
        RETURN; -- 没有水电预留(签约时已结清等)不拦截
    END IF;
    IF r.status <> 'FINALIZED' THEN
        RAISE EXCEPTION 'CONFLICT_UTILITY_PENDING: utility reserve % status=% is not finalized',
            r.id, r.status USING ERRCODE = 'check_violation';
    END IF;
    IF r.shortage_amount > r.topup_received THEN
        RAISE EXCEPTION 'CONFLICT_UTILITY_TOPUP: utility reserve % shortage %.2f > topup received %.2f',
            r.id, r.shortage_amount, r.topup_received USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION trg_settlement_guard() RETURNS trigger AS $$
BEGIN
    IF NEW.status = 'CONFIRMED' AND OLD.status = 'PROPOSED' THEN
        PERFORM assert_no_open_dispute(NEW.contract_id);
        PERFORM assert_utility_reserve_final(NEW.contract_id);
        IF (SELECT hard_hold FROM escrow_accounts WHERE contract_id = NEW.contract_id)
           IS DISTINCT FROM NEW.deducted_total THEN
            RAISE EXCEPTION 'CONFLICT_HOLD_MISMATCH: deducted_total % != hard_hold',
                NEW.deducted_total USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
