package app

import (
	"context"
	"fmt"

	"deposit-escrow/internal/domain"

	"github.com/jackc/pgx/v5"
)

// ---------- 用户/房源(管理与演示) ----------

type CreateUserInput struct {
	Phone       string `json:"phone"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (string, error) {
	if in.Phone == "" || in.DisplayName == "" {
		return "", fmt.Errorf("%w: phone/display_name required", ErrValidation)
	}
	switch in.Role {
	case "TENANT", "LANDLORD", "AGENT", "PLATFORM":
	default:
		return "", fmt.Errorf("%w: bad role", ErrValidation)
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users(phone,display_name,role) VALUES($1,$2,$3) RETURNING id::text`,
		in.Phone, in.DisplayName, in.Role).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}
	return id, nil
}

type CreatePropertyInput struct {
	Address string `json:"address"`
	City    string `json:"city"`
	Rooms   string `json:"rooms"`
}

func (s *Service) CreateProperty(ctx context.Context, in CreatePropertyInput) (string, error) {
	a := actor(ctx)
	if a.Role != "LANDLORD" && a.Role != "AGENT" && a.Role != "PLATFORM" {
		return "", fmt.Errorf("%w: only landlord/agent can create property", ErrForbidden)
	}
	if in.Address == "" || in.City == "" {
		return "", fmt.Errorf("%w: address/city required", ErrValidation)
	}
	var id string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO properties(landlord_id,address,city,rooms) VALUES($1,$2,$3,$4) RETURNING id::text`,
		a.ID, in.Address, in.City, in.Rooms).Scan(&id)
	return id, err
}

// ---------- 签约与押金托管 ----------

type SignContractInput struct {
	PropertyID        string  `json:"property_id"`
	TenantID          string  `json:"tenant_id"`
	AgentID           string  `json:"agent_id"`
	StartDate         string  `json:"start_date"`
	EndDate           string  `json:"end_date"`
	RentAmount        float64 `json:"rent_amount"`
	DepositAmount     float64 `json:"deposit_amount"`
	WearClause        string  `json:"wear_clause"`
	NoticeDays        int     `json:"notice_days"`
	InspectionDueDays int     `json:"inspection_due_days"`
	PaymentDueDays    int     `json:"payment_due_days"`
	LeaseNo           string  `json:"lease_no"`
}

func (s *Service) SignContract(ctx context.Context, in SignContractInput) (string, error) {
	a := actor(ctx)
	if a.Role != "LANDLORD" && a.Role != "AGENT" && a.Role != "PLATFORM" {
		return "", fmt.Errorf("%w: only landlord/agent can sign contract", ErrForbidden)
	}
	if in.PropertyID == "" || in.TenantID == "" || in.StartDate == "" || in.EndDate == "" {
		return "", fmt.Errorf("%w: missing fields", ErrValidation)
	}
	if in.DepositAmount <= 0 || in.RentAmount <= 0 {
		return "", fmt.Errorf("%w: amounts must be positive", ErrValidation)
	}
	if in.NoticeDays <= 0 {
		in.NoticeDays = 30
	}
	if in.InspectionDueDays <= 0 {
		in.InspectionDueDays = 7
	}
	if in.PaymentDueDays <= 0 {
		in.PaymentDueDays = 7
	}
	var id, landlord string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO contracts(lease_no,property_id,landlord_id,tenant_id,agent_id,
			start_date,end_date,rent_amount,deposit_amount,wear_clause,
			notice_days,inspection_due_days,payment_due_days,status)
		SELECT COALESCE(NULLIF($11,''), 'L'||to_char(now(),'YYYYMMDD')||substr(gen_random_uuid()::text,1,6)),
		       $1, (SELECT landlord_id FROM properties WHERE id=$1), $2, NULLIF($3,'')::uuid,
		       $4,$5,$6,$7,NULLIF($8,''),$9,$10,$12,'SIGNED'
		RETURNING id::text, landlord_id::text`,
		in.PropertyID, in.TenantID, in.AgentID, in.StartDate, in.EndDate,
		in.RentAmount, in.DepositAmount, in.WearClause,
		in.NoticeDays, in.InspectionDueDays, in.LeaseNo, in.PaymentDueDays).Scan(&id, &landlord)
	if err != nil {
		return "", fmt.Errorf("sign contract: %w", err)
	}
	if err := s.runTx(ctx, id, func(tx pgx.Tx) error {
		return s.emit(tx, ctx, id, "ContractSigned", map[string]any{
			"lease_no": in.LeaseNo, "deposit_amount": in.DepositAmount,
			"start_date": in.StartDate, "end_date": in.EndDate,
		}, a.ID)
	}); err != nil {
		return "", err
	}
	return id, nil
}

// DepositIn 把押金充入托管账户: SIGNED -> ESCROWED -> LIVING (签约即托管即起租)。
func (s *Service) DepositIn(ctx context.Context, contractID string) error {
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		status, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM")
		if err != nil {
			return err
		}
		if err := checkStatus(status, domain.StatusSigned); err != nil {
			return err
		}
		c, err := s.getContract(tx, ctx, contractID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO escrow_accounts(contract_id,balance) VALUES($1,0)`, contractID)
		if err != nil {
			return fmt.Errorf("open escrow: %w", err)
		}
		if err := s.ledger(tx, ctx, contractID, "DEPOSIT_IN", c.DepositAmount,
			c.DepositAmount, 0, 0, 0, "", "", "租客押金充入托管账户", "deposit:"+contractID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE contracts SET status='LIVING' WHERE id=$1`, contractID); err != nil {
			return err
		}
		if err := s.emit(tx, ctx, contractID, "DepositEscrowed", map[string]any{
			"amount": c.DepositAmount, "status": "LIVING",
		}, actor(ctx).ID); err != nil {
			return err
		}
		return s.notify(tx, ctx, contractID, c.LandlordID, "deposit_escrowed",
			map[string]any{"amount": c.DepositAmount})
	})
}

// ---------- 证据登记 ----------

type EvidenceInput struct {
	Category    string `json:"category"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	SizeBytes   int64  `json:"size_bytes"`
	StorageURI  string `json:"storage_uri"`
	Caption     string `json:"caption"`
}

func (s *Service) RegisterEvidence(ctx context.Context, contractID string, in EvidenceInput) (string, error) {
	status, err := func() (string, error) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return "", err
		}
		defer tx.Rollback(ctx)
		return s.assertParty(tx, ctx, contractID)
	}()
	if err != nil {
		return "", err
	}
	if err := checkStatus(status,
		domain.StatusLiving, domain.StatusCheckoutPending, domain.StatusInspecting,
		domain.StatusDeductionProposed, domain.StatusDisputing, domain.StatusEarlyTerminated); err != nil {
		return "", err
	}
	var id string
	err = s.pool.QueryRow(ctx, `
		INSERT INTO evidence(contract_id,category,content_type,sha256,size_bytes,storage_uri,caption,uploader_id)
		VALUES($1,$2,COALESCE(NULLIF($3,''),'image/jpeg'),$4,$5,$6,$7,$8) RETURNING id::text`,
		contractID, in.Category, in.ContentType, in.SHA256, in.SizeBytes, in.StorageURI, in.Caption,
		actor(ctx).ID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("register evidence: %w", err)
	}
	return id, nil
}

// ---------- 入住交割 ----------

type MeterReading struct {
	Kind      string  `json:"kind"`
	Reading   float64 `json:"reading"`
	UnitPrice float64 `json:"unit_price"`
}

type CheckinInput struct {
	WaterReading *float64 `json:"water_meter_reading"`
	ElecReading  *float64 `json:"elec_meter_reading"`
	GasReading   *float64 `json:"gas_meter_reading"`
	KeysHandover string   `json:"keys_handover"`
	CleaningNote string   `json:"cleaning_note"`
	Note         string   `json:"note"`
	EvidenceIDs  []string `json:"evidence_ids"`
	Inventory    []struct {
		Name         string `json:"name"`
		Category     string `json:"category"`
		BrandModel   string `json:"brand_model"`
		CheckinState string `json:"checkin_state"`
		CheckinNote  string `json:"checkin_note"`
	} `json:"inventory"`
	Meters []MeterReading `json:"meters"`
}

func (s *Service) CompleteCheckin(ctx context.Context, contractID string, in CheckinInput) (string, error) {
	var handoverID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		status, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM")
		if err != nil {
			return err
		}
		if err := checkStatus(status, domain.StatusLiving); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO handovers(contract_id,phase,handed_by,received_by,
				water_meter_reading,elec_meter_reading,gas_meter_reading,keys_handover,cleaning_note,note)
			VALUES($1,'CHECKIN',$2,(SELECT tenant_id FROM contracts WHERE id=$1),$3,$4,$5,$6,$7,$8)
			RETURNING id::text`,
			contractID, actor(ctx).ID, in.WaterReading, in.ElecReading, in.GasReading,
			in.KeysHandover, in.CleaningNote, in.Note).Scan(&handoverID); err != nil {
			return fmt.Errorf("checkin handover: %w", err)
		}
		for _, ev := range in.EvidenceIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO handover_evidence(handover_id,evidence_id) VALUES($1,$2::uuid)`,
				handoverID, ev); err != nil {
				return fmt.Errorf("link evidence: %w", err)
			}
		}
		for _, it := range in.Inventory {
			if _, err := tx.Exec(ctx, `
				INSERT INTO inventory_items(contract_id,name,category,brand_model,checkin_state,checkin_note)
				VALUES($1,$2,$3,$4,$5,$6)`,
				contractID, it.Name, it.Category, it.BrandModel, it.CheckinState, it.CheckinNote); err != nil {
				return fmt.Errorf("inventory item: %w", err)
			}
		}
		meterMap := map[string]MeterReading{}
		for _, m := range in.Meters {
			meterMap[m.Kind] = m
		}
		for _, m := range in.Meters {
			if _, err := tx.Exec(ctx, `
				INSERT INTO utility_readings(contract_id,kind,phase,reading,unit_price,recorded_by)
				VALUES($1,$2,'CHECKIN',$3,$4,$5)`,
				contractID, m.Kind, m.Reading, m.UnitPrice, actor(ctx).ID); err != nil {
				return fmt.Errorf("meter reading: %w", err)
			}
		}
		return s.emit(tx, ctx, contractID, "CheckinCompleted", map[string]any{
			"handover_id": handoverID, "items": len(in.Inventory), "meters": len(meterMap),
		}, actor(ctx).ID)
	})
	return handoverID, err
}

// ---------- 维修报修 ----------

type RepairInput struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	ItemName    string   `json:"item_name"`
	EvidenceIDs []string `json:"evidence_ids"`
}

func (s *Service) SubmitRepair(ctx context.Context, contractID string, in RepairInput) (string, error) {
	var id string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		status, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM")
		if err != nil {
			return err
		}
		if err := checkStatus(status, domain.StatusLiving, domain.StatusEarlyTerminated); err != nil {
			return err
		}
		if in.Title == "" || in.Category == "" {
			return fmt.Errorf("%w: title/category required", ErrValidation)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO repairs(contract_id,item_name,title,description,category,status,submitted_by)
			VALUES($1,$2,$3,$4,$5,'SUBMITTED',$6) RETURNING id::text`,
			contractID, in.ItemName, in.Title, in.Description, in.Category, actor(ctx).ID).Scan(&id); err != nil {
			return err
		}
		for _, ev := range in.EvidenceIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO repair_evidence(repair_id,evidence_id) VALUES($1,$2::uuid)`, id, ev); err != nil {
				return err
			}
		}
		c, _ := s.getContract(tx, ctx, contractID)
		if err := s.notify(tx, ctx, contractID, c.LandlordID, "repair_submitted",
			map[string]any{"title": in.Title, "repair_id": id}); err != nil {
			return err
		}
		return s.emit(tx, ctx, contractID, "RepairSubmitted",
			map[string]any{"repair_id": id, "category": in.Category, "title": in.Title}, actor(ctx).ID)
	})
	return id, err
}

// DecideRepair 房东确认维修责任与费用(关联房屋档案, 退租时作为证据)。
type DecideRepairInput struct {
	Status       string  `json:"status"` // LANDLORD_RESPONSIBLE/TENANT_RESPONSIBLE/SHARED/REJECTED/COMPLETED
	CostAmount   float64 `json:"cost_amount"`
	TenantShare  float64 `json:"tenant_share"`
	DecisionNote string  `json:"decision_note"`
}

func (s *Service) DecideRepair(ctx context.Context, contractID, repairID string, in DecideRepairInput) error {
	switch in.Status {
	case "LANDLORD_RESPONSIBLE", "TENANT_RESPONSIBLE", "SHARED", "REJECTED", "COMPLETED":
	default:
		return fmt.Errorf("%w: bad decision status", ErrValidation)
	}
	if in.TenantShare > in.CostAmount {
		return fmt.Errorf("%w: tenant_share > cost", ErrValidation)
	}
	return s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		if _, err := s.assertParty(tx, ctx, contractID, "LANDLORD", "AGENT", "PLATFORM"); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
			UPDATE repairs SET status=$1, cost_amount=$2, tenant_share=$3, decision_note=$4,
			                   decided_by=$5, decided_at=now()
			WHERE id=$6 AND contract_id=$7 AND status IN ('SUBMITTED','RESPONSIBILITY_PENDING')`,
			in.Status, in.CostAmount, in.TenantShare, in.DecisionNote, actor(ctx).ID, repairID, contractID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("%w: repair not found or already decided", ErrBadState)
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.TenantID, "repair_decided",
			map[string]any{"repair_id": repairID, "status": in.Status, "tenant_share": in.TenantShare})
		return s.emit(tx, ctx, contractID, "RepairDecided", map[string]any{
			"repair_id": repairID, "status": in.Status,
			"cost": in.CostAmount, "tenant_share": in.TenantShare,
		}, actor(ctx).ID)
	})
}

// ---------- 退租交房 / 提前退租 ----------

type CheckoutInput struct {
	WaterReading *float64       `json:"water_meter_reading"`
	ElecReading  *float64       `json:"elec_meter_reading"`
	GasReading   *float64       `json:"gas_meter_reading"`
	KeysReturned string         `json:"keys_returned"`
	CleaningNote string         `json:"cleaning_note"`
	Note         string         `json:"note"`
	EvidenceIDs  []string       `json:"evidence_ids"`
	Meters       []MeterReading `json:"meters"`
	Early        bool           `json:"early"`
}

// RequestCheckout 租客发起退租交房。early=true 表示提前退租。
func (s *Service) RequestCheckout(ctx context.Context, contractID string, in CheckoutInput) (string, error) {
	var handoverID string
	err := s.runTx(ctx, contractID, func(tx pgx.Tx) error {
		status, err := s.assertParty(tx, ctx, contractID, "TENANT", "PLATFORM")
		if err != nil {
			return err
		}
		if err := checkStatus(status, domain.StatusLiving, domain.StatusEarlyTerminated); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO handovers(contract_id,phase,handed_by,received_by,
				water_meter_reading,elec_meter_reading,gas_meter_reading,keys_returned,cleaning_note,note)
			VALUES($1,'CHECKOUT',$2,(SELECT landlord_id FROM contracts WHERE id=$1),$3,$4,$5,$6,$7,$8)
			RETURNING id::text`,
			contractID, actor(ctx).ID, in.WaterReading, in.ElecReading, in.GasReading,
			in.KeysReturned, in.CleaningNote, in.Note).Scan(&handoverID); err != nil {
			return err
		}
		for _, ev := range in.EvidenceIDs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO handover_evidence VALUES($1,$2::uuid)`, handoverID, ev); err != nil {
				return err
			}
		}
		for _, m := range in.Meters {
			if _, err := tx.Exec(ctx, `
				INSERT INTO utility_readings(contract_id,kind,phase,reading,unit_price,recorded_by)
				VALUES($1,$2,'CHECKOUT',$3,$4,$5)
				ON CONFLICT (contract_id,kind,phase) DO UPDATE SET reading=EXCLUDED.reading,
					unit_price=EXCLUDED.unit_price, recorded_by=EXCLUDED.recorded_by, recorded_at=now()`,
				contractID, m.Kind, m.Reading, m.UnitPrice, actor(ctx).ID); err != nil {
				return err
			}
		}
		// 计算区间水电煤应结金额并回写((退租读数-入住读数) * 退租单价, 缺单价沿用入住单价)。
		if _, err := tx.Exec(ctx, `
			UPDATE utility_readings co
			SET billed_amount = ROUND(GREATEST(co.reading - ci.reading, 0)
			                          * COALESCE(NULLIF(co.unit_price,0), ci.unit_price)::numeric, 2)
			FROM utility_readings ci
			WHERE ci.contract_id = $1 AND ci.phase='CHECKIN'
			  AND co.contract_id = $1 AND co.phase='CHECKOUT'
			  AND co.kind = ci.kind`, contractID); err != nil {
			return fmt.Errorf("utility bill calc: %w", err)
		}
		newStatus := domain.StatusCheckoutPending
		if in.Early {
			newStatus = domain.StatusEarlyTerminated
		}
		if _, err := tx.Exec(ctx, `UPDATE contracts SET status=$2 WHERE id=$1`,
			contractID, newStatus); err != nil {
			return err
		}
		c, _ := s.getContract(tx, ctx, contractID)
		_ = s.notify(tx, ctx, contractID, c.LandlordID, "checkout_requested",
			map[string]any{"early": in.Early, "inspection_due_days": c.InspectionDueDays})
		return s.emit(tx, ctx, contractID, "CheckoutRequested", map[string]any{
			"handover_id": handoverID, "early": in.Early, "new_status": newStatus,
		}, actor(ctx).ID)
	})
	return handoverID, err
}
