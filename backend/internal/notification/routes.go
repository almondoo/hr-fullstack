package notification

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	platformauth "github.com/your-org/hr-saas/internal/platform/auth"
	"github.com/your-org/hr-saas/internal/platform/tenantdb"
)

// registerConfig holds optional dependencies for RegisterRoutes.
type registerConfig struct {
	svc *Service
}

// RegisterOption is a functional option for RegisterRoutes.
type RegisterOption func(*registerConfig)

// WithService supplies a pre-built *Service to RegisterRoutes.  Use this from
// the composition root (server.go) to inject the real MailSender and systemDB.
func WithService(svc *Service) RegisterOption {
	return func(c *registerConfig) { c.svc = svc }
}

// RegisterBounceRoutes wires the unauthenticated bounce/complaint webhook endpoints.
//
// webhooksGroup must be an unauthenticated router group (e.g. r.Group("/webhooks"))
// because AWS SNS and SendGrid call these endpoints directly without session cookies.
//
// systemDB is the BYPASSRLS database connection used for cross-tenant delivery
// lookups triggered by bounce/complaint webhooks.
//
// Endpoints:
//
//	POST /webhooks/ses/bounce    — SNS bounce/complaint notifications (SES)
//	POST /webhooks/sendgrid      — SendGrid Event Webhook
func RegisterBounceRoutes(webhooksGroup *gin.RouterGroup, tdb *tenantdb.TenantDB, systemDB *gorm.DB, mailer MailSender, cfg BounceWebhookConfig) {
	svc := NewServiceFull(tdb, mailer, systemDB)
	RegisterBounceWebhookRoutes(webhooksGroup, tdb, svc, cfg)
}

// RegisterRoutes wires the notification platform endpoints.
// The group should already have RequireAuth applied per-route below.
//
// svc is the pre-built Service (constructed by the composition root with the
// real MailSender, systemDB, and ChatSenders).  When nil, a default MockSender
// service is constructed (development / test fallback).
//
// Permission namespaces:
//   - notification:read           — read inbox / templates / deliveries / chat destinations
//   - notification:write          — manage templates / preferences / chat destinations / send chat
//   - notification:read_sensitive — decrypt destination email (sensitive)
//
// Endpoints:
//
//	PUT    /notifications/templates                              — notification:write
//	GET    /notifications/templates                              — notification:read
//	PUT    /notifications/preferences                            — notification:write
//	POST   /notifications/chat/send                              — notification:write
//	PUT    /notifications/chat/destinations                      — notification:write
//	GET    /notifications/chat/destinations                      — notification:read
//	GET    /notifications                                        — notification:read (own inbox)
//	GET    /notifications/unread-count                           — notification:read (own)
//	POST   /notifications/:id/read                               — notification:read (own; item-level)
//	GET    /notifications/:id/deliveries                         — notification:read
//	POST   /notifications/deliveries/:delivery_id/process        — notification:write
//	GET    /notifications/deliveries/:delivery_id/email          — notification:read_sensitive
func RegisterRoutes(rg *gin.RouterGroup, tdb *tenantdb.TenantDB, requireAuth gin.HandlerFunc, opts ...RegisterOption) {
	cfg := registerConfig{}
	for _, o := range opts {
		o(&cfg)
	}

	var svc *Service
	if cfg.svc != nil {
		svc = cfg.svc
	} else {
		svc = NewService(tdb)
	}
	h := NewHandler(svc)

	read := platformauth.RequirePermission(tdb, "notification:read")
	write := platformauth.RequirePermission(tdb, "notification:write")
	readSensitive := platformauth.RequirePermission(tdb, "notification:read_sensitive")

	grp := rg.Group("/notifications")
	grp.Use(requireAuth)

	// Templates.
	grp.PUT("/templates", write, h.UpsertTemplate)
	grp.GET("/templates", read, h.ListTemplates)

	// Preferences.
	grp.PUT("/preferences", write, h.SetPreference)

	// Chat: send + destination config.
	// Registered before the /:id wildcard group so "/chat/..." paths resolve
	// correctly without ambiguity.
	chatGrp := grp.Group("/chat")
	chatGrp.POST("/send", write, h.SendChat)
	chatGrp.PUT("/destinations", write, h.UpsertChatDestination)
	chatGrp.GET("/destinations", read, h.ListChatDestinations)

	// Email delivery lifecycle (specific paths registered before /:id wildcards).
	grp.POST("/deliveries/:delivery_id/process", write, h.ProcessDelivery)
	grp.GET("/deliveries/:delivery_id/email", readSensitive, h.GetDeliveryEmail)

	// In-app inbox (own).
	grp.GET("", read, h.ListInbox)
	grp.GET("/unread-count", read, h.UnreadCount)
	grp.POST("/:id/read", read, h.MarkRead)
	grp.GET("/:id/deliveries", read, h.ListDeliveries)
}
