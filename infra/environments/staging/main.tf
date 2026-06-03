# environments/staging/main.tf
#
# ステージング環境 root モジュール
# TODO(#9 GAP-01): 本番環境と同じプロバイダ構成。コストを抑えるため
# インスタンスサイズを小さめに設定する。

# 設計方針:
# - 本番と同一のインフラ構成 (ミラー環境) でデプロイ検証
# - DB は min_instances=1, backup_retention_days=7 程度
# - シークレットは staging 専用のものを使用 (本番シークレットは流用しない)

# TODO(#9): 本番の main.tf と同様に module ブロックを追加する
