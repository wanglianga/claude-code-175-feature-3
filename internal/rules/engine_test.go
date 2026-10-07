package rules

import (
	"math"
	"testing"
)

func findFact(r Report, code string) *Suggestion {
	for i := range r.Facts {
		if r.Facts[i].Code == code {
			return &r.Facts[i]
		}
	}
	return nil
}

func findCharge(r Report, code string) *ChargeSuggestion {
	for i := range r.Charges {
		if r.Charges[i].Code == code {
			return &r.Charges[i]
		}
	}
	return nil
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.005 }

func TestWallNormalWearIsZero(t *testing.T) {
	r := Generate(Snapshot{WallStatus: "NORMAL_WEAR", WallRepairFee: 1000})
	f := findFact(r, CodeWallNormalWear)
	if f == nil || f.Deductible || f.Amount != 0 {
		t.Fatalf("自然磨损应产生 0 元建议, got %+v", f)
	}
}

func TestPreExistingDamageExempt(t *testing.T) {
	r := Generate(Snapshot{
		WallStatus:    "DAMAGED",
		WallRepairFee: 1000,
		Inventory:     []InventoryView{{Name: "墙", Category: "WALL", CheckinState: "DAMAGED"}},
	})
	f := findFact(r, CodePreExistingDamage)
	if f == nil || f.Deductible {
		t.Fatalf("入住前已损坏应免责, got %+v", f)
	}
}

func TestWornCheckinAlsoExempt(t *testing.T) {
	r := Generate(Snapshot{
		FloorStatus:    "DAMAGED",
		FloorRepairFee: 800,
		Inventory:      []InventoryView{{Name: "地板", Category: "FLOOR", CheckinState: "WORN"}},
	})
	if got := r.DeductibleTotal(); got != 0 {
		t.Fatalf("入住基线 WORN 的地板损坏应免责, total=%.2f", got)
	}
}

func TestGoodCheckinDoesNotExempt(t *testing.T) {
	r := Generate(Snapshot{
		WallStatus:    "DAMAGED",
		WallRepairFee: 1000,
		Inventory:     []InventoryView{{Name: "墙", Category: "WALL", CheckinState: "GOOD"}},
	})
	f := findFact(r, CodeWallDamage)
	if f == nil || !f.Deductible || !approx(f.Amount, 1000) {
		t.Fatalf("入住基线 GOOD 的新损应按报价扣款, got %+v", f)
	}
}

func TestRepairTenantResponsible(t *testing.T) {
	r := Generate(Snapshot{
		WallStatus:    "DAMAGED",
		WallRepairFee: 1000,
		Inventory:     []InventoryView{{Name: "墙", Category: "WALL", CheckinState: "GOOD"}},
		Repairs: []RepairView{{
			ID: "r1", Category: "WALL", Status: "TENANT_RESPONSIBLE",
			CostAmount: 800, TenantShare: 800,
		}},
	})
	f := findFact(r, CodeWallDamage)
	if f == nil || f.LinkedRepairID != "r1" || !approx(f.Amount, 800) {
		t.Fatalf("租客责任维修应按 tenant_share 扣款, got %+v", f)
	}
}

func TestRepairLandlordResponsibleExempt(t *testing.T) {
	r := Generate(Snapshot{
		WallStatus: "DAMAGED", WallRepairFee: 1000,
		Inventory: []InventoryView{{Name: "墙", Category: "WALL", CheckinState: "GOOD"}},
		Repairs:   []RepairView{{ID: "r2", Category: "WALL", Status: "LANDLORD_RESPONSIBLE"}},
	})
	if got := r.DeductibleTotal(); got != 0 {
		t.Fatalf("房东责任维修应免责, total=%.2f", got)
	}
}

func TestSharedResponsibilityIsHalf(t *testing.T) {
	r := Generate(Snapshot{
		FloorStatus: "DAMAGED", FloorRepairFee: 1000,
		Inventory: []InventoryView{{Name: "地板", Category: "FLOOR", CheckinState: "GOOD"}},
		Repairs:   []RepairView{{ID: "r3", Category: "FLOOR", Status: "SHARED", CostAmount: 900}},
	})
	f := findFact(r, CodeFloorDamage)
	if f == nil || !approx(f.Amount, 450) {
		t.Fatalf("责任共担应为费用一半 450, got %+v", f)
	}
}

func TestApplianceUnclearWithoutRepairRecord(t *testing.T) {
	r := Generate(Snapshot{
		ApplianceStatus: "DAMAGED", ApplianceFee: 2000,
		Inventory: []InventoryView{{Name: "冰箱", Category: "APPLIANCE", CheckinState: "GOOD"}},
	})
	f := findFact(r, CodeApplianceUnclear)
	if f == nil || !f.Deductible {
		t.Fatalf("无维修记录应判责任不清, got %+v", f)
	}
}

func TestApplianceUnclearWithUndecidedRepair(t *testing.T) {
	r := Generate(Snapshot{
		ApplianceStatus: "MISSING", ApplianceFee: 3000,
		Inventory: []InventoryView{{Name: "洗衣机", Category: "APPLIANCE", CheckinState: "GOOD"}},
		Repairs:   []RepairView{{ID: "r4", Category: "APPLIANCE", Status: "SUBMITTED"}},
	})
	f := findFact(r, CodeApplianceUnclear)
	if f == nil || f.LinkedRepairID != "r4" {
		t.Fatalf("未定论工单应保持责任不清并关联工单, got %+v", f)
	}
}

func TestEarlyTermination(t *testing.T) {
	r := Generate(Snapshot{
		DepositAmount: 10000, RentAmount: 6000, NoticeDays: 30,
		EarlyTerminated: true,
		WallStatus:      "GOOD", FloorStatus: "GOOD", ApplianceStatus: "GOOD",
		KeysStatus: "RETURNED", UtilityStatus: "SETTLED", CleaningStatus: "CLEAN",
	})
	f := findFact(r, CodeEarlyTerminationLiability)
	if f == nil || !approx(f.Amount, 6000) {
		t.Fatalf("30天通知期月租6000应建议6000, got %+v", f)
	}
}

func TestChargesAndTotal(t *testing.T) {
	r := Generate(Snapshot{
		DepositAmount: 10000,
		KeysStatus:    "MISSING", KeyReplacementFee: 350,
		UtilityStatus: "UNPAID", UtilityBilledTotal: 265,
		CleaningStatus: "NEEDS_CLEANING", CleaningFee: 600,
	})
	if findCharge(r, ChargeKeyLoss) == nil {
		t.Fatal("应有钥匙补办费")
	}
	if findCharge(r, ChargeUtilityArrears) == nil {
		t.Fatal("应有水电欠费")
	}
	if findCharge(r, ChargeCleaning) == nil {
		t.Fatal("应有清洁费")
	}
	if !approx(r.DeductibleTotal(), 1215) {
		t.Fatalf("合计应为 1215, got %.2f", r.DeductibleTotal())
	}
}

func TestCapByDeposit(t *testing.T) {
	r := Generate(Snapshot{
		DepositAmount: 500,
		KeysStatus:    "MISSING", KeyReplacementFee: 300,
		UtilityStatus: "UNPAID", UtilityBilledTotal: 400,
		CleaningStatus: "CLEAN",
	})
	if r.DeductibleTotal() > 500+0.001 {
		t.Fatalf("扣款总额不得超过押金 500, got %.2f", r.DeductibleTotal())
	}
	if len(r.Notes) == 0 {
		t.Fatal("超押金截断时应给出提示")
	}
}

func TestApplianceMissingAtCheckinExempt(t *testing.T) {
	r := Generate(Snapshot{
		ApplianceStatus: "MISSING", ApplianceFee: 2000,
		Inventory: []InventoryView{{Name: "电视", Category: "APPLIANCE", CheckinState: "MISSING"}},
	})
	if f := findFact(r, CodePreExistingDamage); f == nil || f.Deductible {
		t.Fatalf("入住时即缺失的家电应免责, got %+v", f)
	}
}

// TestUtilityReservedSkipsArrears 退租时已为最后一期未出账账单预留押金的,
// 验房引擎不得再生成水电欠费建议, 避免与预留/最终结算双重扣款。
func TestUtilityReservedSkipsArrears(t *testing.T) {
	base := Snapshot{
		UtilityStatus: "UNPAID", UtilityBilledTotal: 265,
		CleaningStatus: "CLEAN", KeysStatus: "RETURNED",
		WallStatus: "GOOD", FloorStatus: "GOOD", ApplianceStatus: "GOOD",
		DepositAmount: 10000,
	}
	without := Generate(base)
	if findCharge(without, ChargeUtilityArrears) == nil {
		t.Fatal("未预留时应生成水电欠费建议")
	}
	with := base
	with.UtilityReserved = true
	r := Generate(with)
	if findCharge(r, ChargeUtilityArrears) != nil {
		t.Fatalf("已预留后不应再生成水电欠费建议, got %+v", r.Charges)
	}
	if r.DeductibleTotal() != 0 {
		t.Fatalf("已预留且无其他损坏时建议合计应为 0, got %.2f", r.DeductibleTotal())
	}
}
