# =============================================================================
# 本番用マルチステージ Dockerfile
# - ステージ 1 (builder): Go バイナリをビルド（静的リンク）
# - ステージ 2 (final):   distroless/static-debian12 — シェル/パッケージマネージャなし
#   非 root ユーザー (uid=65532) で実行
#
# 注意: このファイルは本番向けです。開発環境には Dockerfile.dev を使ってください。
# =============================================================================

# --- Stage 1: Builder ---
FROM golang:1.26-alpine AS builder

# CGO 不要（純 Go ライブラリのみ使用）
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

# ビルド時に必要な最小ツールのみ追加
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# 依存解決を先に行いキャッシュ効率を高める
COPY go.mod go.sum ./
RUN go mod download

# ソースコードをコピーしてビルド
COPY . .

# 静的バイナリ生成。trimpath でパス情報を除去し、不要なデバッグ情報を削除
RUN go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/server \
      ./main.go

# --- Stage 2: Final (distroless) ---
# distroless/static-debian12: シェルなし・パッケージなし・最小攻撃面
# 非 root uid=65532 (nonroot) は Google distroless のデフォルト
FROM gcr.io/distroless/static-debian12:nonroot AS final

# タイムゾーン情報とCA証明書を builder から取得
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# バイナリをコピー
COPY --from=builder /out/server /server

# 非 root ユーザーで実行（distroless nonroot image のデフォルト: uid=65532）
# Note: db/migrations は backend/db/migrations.go の //go:embed により
# バイナリに静的埋め込み済みのため、別途コピー不要。
USER nonroot:nonroot

EXPOSE 8080

# ヘルスチェック: /healthz エンドポイント
# distroless にはシェルがないため wget/curl は使用不可。
# Kubernetes liveness/readiness probe を外部から設定すること。
# (参考: HEALTHCHECK は docker-compose や外部オーケストレーターに委ねる)

ENTRYPOINT ["/server"]
