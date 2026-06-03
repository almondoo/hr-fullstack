package billing

import (
	"github.com/gin-gonic/gin"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// RegisterRoutes wires the billing endpoints onto rg.  The group already has
// RequireAuth applied per-subgroup; each endpoint additionally enforces an
// RBAC permission via RequirePermission.
//
// Endpoints (permission):
//
//	GET    /billing/plans                                  — billing:read
//	POST   /billing/subscriptions                          — billing:write
//	GET    /billing/subscriptions/current                  — billing:read
//	PATCH  /billing/subscriptions/current/status           — billing:write
//	POST   /billing/subscriptions/current/cancel           — billing:write
//	POST   /billing/subscriptions/current/seat-usage       — billing:write
//	POST   /billing/invoices                               — billing:write
//	GET    /billing/invoices                               — billing:read
//	GET    /billing/invoices/:id                           — billing:read
//	POST   /billing/invoices/:id/pay                       — billing:write
//	GET    /billing/invoices/:id/payments                  — billing:read
//	POST   /billing/provisioning                           — billing:write
//	GET    /billing/provisioning                           — billing:read
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc) {
	svc := NewService(tdb)
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "billing:read")
	write := platformauth.RequirePermission(tdb, "billing:write")

	grp := rg.Group("/billing")
	grp.Use(requireAuth)
	grp.GET("/plans", read, h.ListPlans)

	grp.POST("/subscriptions", write, h.CreateSubscription)
	grp.GET("/subscriptions/current", read, h.GetSubscription)
	grp.PATCH("/subscriptions/current/status", write, h.ChangeSubscriptionStatus)
	grp.POST("/subscriptions/current/cancel", write, h.CancelSubscription)
	grp.POST("/subscriptions/current/seat-usage", write, h.CaptureSeatUsage)

	grp.POST("/invoices", write, h.GenerateInvoice)
	grp.GET("/invoices", read, h.ListInvoices)
	grp.GET("/invoices/:id", read, h.GetInvoice)
	grp.POST("/invoices/:id/pay", write, h.PayInvoice)
	grp.GET("/invoices/:id/payments", read, h.ListPaymentAttempts)

	grp.POST("/provisioning", write, h.ProvisionTenant)
	grp.GET("/provisioning", read, h.GetProvisioning)
}
