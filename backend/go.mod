module github.com/your-org/hr-saas

go 1.26

// 依存（gin, gorm, gorm postgres driver 等）は `go mod tidy` で自動解決されます。
// 本番では go.sum をコミットし、govulncheck を CI で必須化してください。
