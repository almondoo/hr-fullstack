package server

import (
	"context"
	"fmt"

	"github.com/your-org/hr-saas/internal/govfiling"
	"github.com/your-org/hr-saas/internal/mynumber"
)

// mynumberProviderAdapter bridges govfiling.MynumberProvider to mynumber.Service.
//
// govfiling defines a MynumberProvider interface to avoid a direct dependency on
// the mynumber package (dependency inversion).  This adapter lives in the server
// package — the only package that already imports both domains — and translates
// between govfiling.MynumberProvideInput and mynumber.ProvideInput.
//
// Security: the adapter passes through only opaque IDs; it never copies, stores,
// or logs the returned plaintext bytes.
type mynumberProviderAdapter struct {
	svc *mynumber.Service
}

// NewMynumberProviderAdapter wraps a *mynumber.Service as a govfiling.MynumberProvider.
func NewMynumberProviderAdapter(svc *mynumber.Service) govfiling.MynumberProvider {
	return &mynumberProviderAdapter{svc: svc}
}

// Provide translates govfiling.MynumberProvideInput to mynumber.ProvideInput and
// delegates to mynumber.Service.Provide.  The returned bytes are the plaintext
// 個人番号; the caller (govfiling.Service.SubmitFiling) is responsible for zeroing
// and discarding the value after use.
func (a *mynumberProviderAdapter) Provide(ctx context.Context, in govfiling.MynumberProvideInput) ([]byte, error) {
	plain, err := a.svc.Provide(ctx, mynumber.ProvideInput{
		TenantID:   in.TenantID,
		ActorID:    in.ActorID,
		RecordID:   in.RecordID,
		Purpose:    in.Purpose,
		ProvidedTo: in.ProvidedTo,
		IP:         in.IP,
	})
	if err != nil {
		return nil, fmt.Errorf("mynumber adapter: provide: %w", err)
	}
	return plain, nil
}
