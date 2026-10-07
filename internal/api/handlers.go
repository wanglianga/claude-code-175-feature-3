package api

import (
	"net/http"
	"strconv"

	"deposit-escrow/internal/app"
)

// Routes 注册全部 HTTP 路由。
func (s *Server) Routes() *Router {
	r := NewRouter()

	// 文档与健康检查
	r.Handle("GET", "healthz", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	r.Handle("GET", "readyz", s.ready)
	s.mountSwagger(r)

	// 认证
	r.Handle("POST", "api/v1/auth/login", s.login)

	// 用户/房源
	r.Handle("POST", "api/v1/users", s.withAuth(s.createUser))
	r.Handle("POST", "api/v1/properties", s.withAuth(s.createProperty))

	// 合同与租赁全生命周期
	r.Handle("GET", "api/v1/contracts", s.withAuth(s.listContracts))
	r.Handle("POST", "api/v1/contracts", s.withAuth(s.signContract))
	r.Handle("GET", "api/v1/contracts/{id}", s.withAuth(s.getTimeline))
	r.Handle("GET", "api/v1/contracts/{id}/timeline", s.withAuth(s.getTimeline))

	r.Handle("POST", "api/v1/contracts/{id}/deposit", s.withAuth(s.depositIn))
	r.Handle("POST", "api/v1/contracts/{id}/evidence", s.withAuth(s.registerEvidence))
	r.Handle("POST", "api/v1/contracts/{id}/checkin", s.withAuth(s.completeCheckin))

	r.Handle("POST", "api/v1/contracts/{id}/repairs", s.withAuth(s.submitRepair))
	r.Handle("POST", "api/v1/contracts/{id}/repairs/{rid}/decision", s.withAuth(s.decideRepair))

	r.Handle("POST", "api/v1/contracts/{id}/checkout", s.withAuth(s.requestCheckout))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/start", s.withAuth(s.startInspection))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/submit", s.withAuth(s.submitInspection))
	r.Handle("POST", "api/v1/contracts/{id}/inspection/review", s.withAuth(s.reviewInspection))

	r.Handle("POST", "api/v1/contracts/{id}/items/{itype}/{iid}/accept", s.withAuth(s.acceptItem))
	r.Handle("POST", "api/v1/contracts/{id}/items/{itype}/{iid}/contest", s.withAuth(s.contestItem))

	r.Handle("POST", "api/v1/contracts/{id}/disputes/inspection-delay", s.withAuth(s.openDelayDispute))
	r.Handle("POST", "api/v1/contracts/{id}/disputes/{did}/actions", s.withAuth(s.disputeAction))
	r.Handle("GET", "api/v1/contracts/{id}/disputes", s.withAuth(s.listDisputes))

	r.Handle("POST", "api/v1/contracts/{id}/settlement/propose", s.withAuth(s.proposeSettlement))
	r.Handle("POST", "api/v1/contracts/{id}/settlement/confirm", s.withAuth(s.confirmSettlement))

	// 退租水电尾款结算: 预留 -> 供应商账单 -> 照片复核 -> 人工复核 -> 唯一一笔最终押金流水
	r.Handle("POST", "api/v1/contracts/{id}/utility-reservation", s.withAuth(s.createUtilityReservation))
	r.Handle("POST", "api/v1/contracts/{id}/utility-reservation/{rid}/bills", s.withAuth(s.submitSupplierBill))
	r.Handle("POST", "api/v1/contracts/{id}/utility-reservation/{rid}/meter-photos", s.withAuth(s.submitMeterPhotos))
	r.Handle("POST", "api/v1/contracts/{id}/utility-reservation/{rid}/review", s.withAuth(s.reviewMeter))
	r.Handle("POST", "api/v1/contracts/{id}/utility-reservation/{rid}/dispute", s.withAuth(s.openUtilityDispute))
	r.Handle("POST", "api/v1/contracts/{id}/utility-reservation/{rid}/manual-resolve", s.withAuth(s.resolveUtilityManual))
	r.Handle("POST", "api/v1/contracts/{id}/utility-reservation/{rid}/finalize", s.withAuth(s.finalizeUtility))
	r.Handle("POST", "api/v1/contracts/{id}/utility-supplements/{sid}/pay", s.withAuth(s.paySupplement))

	// 平台: 逾期未验房巡检 / 水电账单延迟巡检
	r.Handle("POST", "api/v1/admin/overdue-inspections", s.withAuth(s.autoOverdue))
	r.Handle("POST", "api/v1/admin/utility-bill-delay", s.withAuth(s.autoUtilityBillDelay))

	return r
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phone string `json:"phone"`
	}
	if err := decode(r, &in); err != nil || in.Phone == "" {
		writeJSON(w, 400, map[string]string{"error": "VALIDATION", "message": "phone required"})
		return
	}
	sess, err := s.auth.Login(r.Context(), in.Phone)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"token": sess.Token, "actor": sess.Actor})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CreateUserInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.CreateUser(r.Context(), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (s *Server) createProperty(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CreatePropertyInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.CreateProperty(app.WithActor(r.Context(), a), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (s *Server) listContracts(w http.ResponseWriter, r *http.Request, a app.Actor) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rs, err := s.q.ListContracts(r.Context(), a.Role, a.ID, limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": rs})
}

func (s *Server) signContract(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.SignContractInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.SignContract(app.WithActor(r.Context(), a), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id, "status": "SIGNED"})
}

func (s *Server) getTimeline(w http.ResponseWriter, r *http.Request, a app.Actor) {
	id := Param(r, "id")
	if err := s.q.AssertContractParty(r.Context(), id, a); err != nil {
		writeErr(w, err)
		return
	}
	tl, err := s.q.LeaseTimeline(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, tl)
}

func (s *Server) depositIn(w http.ResponseWriter, r *http.Request, a app.Actor) {
	err := s.svc.DepositIn(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "LIVING"})
}

func (s *Server) registerEvidence(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.EvidenceInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.RegisterEvidence(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"id": id})
}

func (s *Server) completeCheckin(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CheckinInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.CompleteCheckin(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"handover_id": id, "phase": "CHECKIN"})
}

func (s *Server) submitRepair(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.RepairInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.SubmitRepair(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"repair_id": id, "status": "SUBMITTED"})
}

func (s *Server) decideRepair(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.DecideRepairInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.DecideRepair(app.WithActor(r.Context(), a), Param(r, "id"), Param(r, "rid"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": in.Status})
}

func (s *Server) requestCheckout(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CheckoutInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.RequestCheckout(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"handover_id": id})
}

func (s *Server) startInspection(w http.ResponseWriter, r *http.Request, a app.Actor) {
	id, err := s.svc.StartInspection(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"inspection_id": id, "status": "OPEN"})
}

func (s *Server) submitInspection(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.SubmitInspectionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	rep, err := s.svc.SubmitInspection(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"status":           "DEDUCTION_PROPOSED",
		"deductible_total": rep.DeductibleTotal(),
		"facts":            rep.Facts,
		"charges":          rep.Charges,
		"notes":            rep.Notes,
	})
}

func (s *Server) reviewInspection(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Agree   bool   `json:"agree"`
		Comment string `json:"comment"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.TenantReviewInspection(app.WithActor(r.Context(), a), Param(r, "id"), in.Agree, in.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) acceptItem(w http.ResponseWriter, r *http.Request, a app.Actor) {
	err := s.svc.AcceptItem(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "itype"), Param(r, "iid"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ACCEPTED"})
}

func (s *Server) contestItem(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.ContestItemInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.ContestItem(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "itype"), Param(r, "iid"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"dispute_id": id, "status": "OPEN"})
}

func (s *Server) openDelayDispute(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Comment string `json:"comment"`
	}
	_ = decode(r, &in)
	id, err := s.svc.OpenInspectionDelayDispute(app.WithActor(r.Context(), a), Param(r, "id"), in.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"dispute_id": id, "reason": "INSPECTION_DELAY"})
}

func (s *Server) disputeAction(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.DisputeActionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	err := s.svc.DisputeAction(app.WithActor(r.Context(), a), Param(r, "id"), Param(r, "did"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listDisputes(w http.ResponseWriter, r *http.Request, a app.Actor) {
	cid := Param(r, "id")
	if err := s.q.AssertContractParty(r.Context(), cid, a); err != nil {
		writeErr(w, err)
		return
	}
	tl, err := s.q.LeaseTimeline(r.Context(), cid)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": tl.Disputes, "actions": tl.Actions})
}

func (s *Server) proposeSettlement(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Note string `json:"note"`
	}
	_ = decode(r, &in)
	id, err := s.svc.ProposeSettlement(app.WithActor(r.Context(), a), Param(r, "id"), in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"settlement_id": id, "status": "PROPOSED"})
}

func (s *Server) confirmSettlement(w http.ResponseWriter, r *http.Request, a app.Actor) {
	err := s.svc.ConfirmSettlement(app.WithActor(r.Context(), a), Param(r, "id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) autoOverdue(w http.ResponseWriter, r *http.Request, a app.Actor) {
	ids, err := s.svc.AutoOpenOverdueInspection(app.WithActor(r.Context(), a))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"opened": ids})
}

// ---------- 退租水电尾款结算 ----------

func (s *Server) createUtilityReservation(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.CreateUtilityReservationInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	rid, err := s.svc.CreateUtilityReservation(app.WithActor(r.Context(), a), Param(r, "id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{"reservation_id": rid, "status": "RESERVED"})
}

func (s *Server) submitSupplierBill(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.SubmitSupplierBillInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	rid, dup, err := s.svc.SubmitSupplierBill(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "rid"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := 201
	if dup {
		status = 200
	}
	writeJSON(w, status, map[string]any{
		"bill_id": rid, "duplicated": dup,
		"message": map[bool]string{true: "同一账单号已登记, 幂等返回, 未重复扣款"}[dup],
	})
}

func (s *Server) submitMeterPhotos(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.SubmitMeterPhotosInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.svc.SubmitMeterPhotos(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "rid"), in); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "message": "照片读数已登记, 将与供应商账单交叉复核, 不会只按账单扣款"})
}

func (s *Server) reviewMeter(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.ReviewMeterInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	id, err := s.svc.ReviewMeterEvidence(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "rid"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"meter_review_id": id, "decision": in.Decision})
}

func (s *Server) openUtilityDispute(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Comment string `json:"comment"`
	}
	_ = decode(r, &in)
	id, err := s.svc.OpenUtilityAllocationDispute(app.WithActor(r.Context(), a),
		Param(r, "id"), in.Comment)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 201, map[string]string{
		"dispute_id": id, "reason": "UTILITY_FINAL_SETTLEMENT",
		"status": "MANUAL_REVIEW",
	})
}

func (s *Server) resolveUtilityManual(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in app.ResolveUtilityManualInput
	if err := decode(r, &in); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.svc.ResolveUtilityManual(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "rid"), in); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "BILL_RECEIVED"})
}

func (s *Server) finalizeUtility(w http.ResponseWriter, r *http.Request, a app.Actor) {
	var in struct {
		Note string `json:"note"`
	}
	_ = decode(r, &in)
	res, err := s.svc.SettleUtilityReservation(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "rid"), in.Note)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := 201
	if res.Idempotent {
		status = 200
	}
	writeJSON(w, status, res)
}

func (s *Server) paySupplement(w http.ResponseWriter, r *http.Request, a app.Actor) {
	if err := s.svc.PayUtilitySupplement(app.WithActor(r.Context(), a),
		Param(r, "id"), Param(r, "sid")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "PAID"})
}

func (s *Server) autoUtilityBillDelay(w http.ResponseWriter, r *http.Request, a app.Actor) {
	ids, err := s.svc.MarkUtilityBillDelayed(app.WithActor(r.Context(), a))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"manual_review": ids})
}
