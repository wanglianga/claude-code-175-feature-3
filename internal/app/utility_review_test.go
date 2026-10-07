package app

import (
	"testing"

	"deposit-escrow/internal/domain"
)

func item(kind string, hasBill bool, billReading, billedAmt float64,
	hasPhoto bool, photoReading, photoAmt float64, split bool) UtilityReviewItem {
	it := UtilityReviewItem{
		Kind: kind, CheckinReading: 100, LastReading: 112,
		Tolerance: readingTolerance, SplitDisputed: split,
	}
	if hasBill {
		it.HasBill, it.BillReading, it.BilledAmount = true, billReading, billedAmt
	}
	if hasPhoto {
		it.HasPhoto, it.PhotoReading, it.PhotoAmount = true, photoReading, photoAmt
	}
	return it
}

func TestReview_BillOnly(t *testing.T) {
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("WATER", true, 112, 60, false, 0, 0, false),
		item("ELECTRIC", true, 2300, 180, false, 0, 0, false),
	})
	if d.Blocker != "" {
		t.Fatalf("expected no blocker, got %s", d.Blocker)
	}
	if d.Decision != "BILL" {
		t.Fatalf("expected decision BILL, got %s", d.Decision)
	}
	if d.FinalTotal != 240 {
		t.Fatalf("expected final 240, got %.2f", d.FinalTotal)
	}
}

func TestReview_PhotoMatchesBill(t *testing.T) {
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("WATER", true, 112, 60, true, 112, 60, false),
	})
	if d.Blocker != "" {
		t.Fatalf("expected no blocker, got %s", d.Blocker)
	}
	if d.Decision != "PHOTO" {
		t.Fatalf("expected PHOTO decision (all kinds have photos), got %s", d.Decision)
	}
	if d.FinalAmounts["WATER"] != 60 {
		t.Fatalf("expected water final 60, got %.2f", d.FinalAmounts["WATER"])
	}
}

func TestReview_PhotoWithinTolerance(t *testing.T) {
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("WATER", true, 112, 60, true, 112.5, 62, false),
	})
	if d.Blocker != "" {
		t.Fatalf("reading within tolerance must correspond, got blocker %s", d.Blocker)
	}
}

func TestReview_BillDelayed(t *testing.T) {
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("WATER", true, 112, 60, true, 112, 60, false),
		item("ELECTRIC", false, 0, 0, false, 0, 0, false),
	})
	if d.Blocker != domain.UtilityFlagBillDelay {
		t.Fatalf("expected BILL_DELAY, got %q", d.Blocker)
	}
	if d.Flags["ELECTRIC"] != domain.UtilityFlagBillDelay {
		t.Fatalf("expected ELECTRIC flag BILL_DELAY, got %q", d.Flags["ELECTRIC"])
	}
	// 即使 WATER 已有账单, 整体存在 blocker, 调用方不会执行 finalize, 预留款全额挂账。
	if d.Decision != "" {
		t.Fatalf("blocked review must not produce a decision, got %q", d.Decision)
	}
}

func TestReview_BillReadingBelowLast(t *testing.T) {
	// 账单读数 105 低于搬离最近读数 112 超过容差 -> 读数无法对应。
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("WATER", true, 105, 40, false, 0, 0, false),
	})
	if d.Blocker != domain.UtilityFlagReadingMismatch {
		t.Fatalf("expected READING_MISMATCH, got %q", d.Blocker)
	}
	if d.Decision != "" {
		t.Fatalf("blocked review must not decide, got %q", d.Decision)
	}
}

func TestReview_PhotoContradictsBill(t *testing.T) {
	// 不能只按账单扣款: 账单读数 130 与搬离当天照片 112 相差 18 > 容差 -> 人工复核。
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("WATER", true, 130, 150, true, 112, 60, false),
	})
	if d.Blocker != domain.UtilityFlagReadingMismatch {
		t.Fatalf("expected READING_MISMATCH (photo vs bill), got %q", d.Blocker)
	}
	if d.FinalAmounts["WATER"] != 0 {
		t.Fatalf("contradicted bill amount must not be charged, got %.2f", d.FinalAmounts["WATER"])
	}
}

func TestReview_SplitDisputeBlocksEvenWithBillAndPhoto(t *testing.T) {
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("WATER", true, 112, 60, true, 112, 60, true),
	})
	if d.Blocker != domain.UtilityFlagSplitDispute {
		t.Fatalf("expected SPLIT_DISPUTE to block, got %q", d.Blocker)
	}
}

func TestReview_DisputeTakesPriorityOverDelay(t *testing.T) {
	d := ReviewUtilityItems([]UtilityReviewItem{
		item("ELECTRIC", false, 0, 0, false, 0, 0, false), // 账单延迟
		item("WATER", true, 112, 60, false, 0, 0, true),   // 分摊争议
	})
	if d.Blocker != domain.UtilityFlagSplitDispute {
		t.Fatalf("SPLIT_DISPUTE must outrank BILL_DELAY, got %q", d.Blocker)
	}
}

func TestEstimateUtilityReserve(t *testing.T) {
	// 100->112 共 30 天, 单价 5: 月均用量 12, 预留 60。
	got := EstimateUtilityReserve(100, 112, 5, 30)
	if got != 60 {
		t.Fatalf("expected reserve 60, got %.2f", got)
	}
	// 短租不足一个月按一个月计: 100->105 共 10 天 -> 月均按 5 -> 25。
	got = EstimateUtilityReserve(100, 105, 5, 10)
	if got != 25 {
		t.Fatalf("expected short-tenancy reserve 25, got %.2f", got)
	}
}
