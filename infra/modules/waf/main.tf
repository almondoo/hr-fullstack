# modules/waf/main.tf
#
# AWS WAFv2 WebACL — ALB アタッチ / マネージドルール / レートベースルール
# Issue #17 (WAF/レート制限/DDoS)
#
# 設計方針:
#   - WAFv2 WebACL を ALB にアタッチ (REGIONAL スコープ)
#   - AWSManagedRulesCommonRuleSet (OWASP CRS 相当)
#   - AWSManagedRulesKnownBadInputsRuleSet (既知の攻撃入力)
#   - AWSManagedRulesSQLiRuleSet (SQLインジェクション)
#   - レートベースルール: 同一 IP から過剰リクエストをブロック (#17)
#   - IP 許可リスト / 拒否リストを変数で制御
#   - Shield Standard 前提 (Shield Advanced は別途契約が必要)
#
# Shield Standard について:
#   AWS Shield Standard はすべての AWS アカウントで自動的に有効。
#   L3/L4 DDoS 攻撃 (SYN flood, UDP reflection 等) を自動ブロック。
#   L7 DDoS 対策 (HTTP flood) は WAFv2 レートルールで対応。
#   Shield Advanced (有料) が必要な場合は別途 aws_shield_protection を追加すること。

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

# ---------------------------------------------------------------------------
# 変数
# ---------------------------------------------------------------------------

variable "environment" {
  type        = string
  description = "環境名 (production / staging / development)"
}

variable "alb_arn" {
  type        = string
  description = "WAF WebACL をアタッチする ALB の ARN"
}

variable "rate_limit_per_ip" {
  type        = number
  description = "レートベースルール: 5 分間の IP あたり最大リクエスト数 (デフォルト 300 = 60 rps)"
  default     = 300
  # NOTE: WAFv2 レートルールは 5 分間のウィンドウで評価される。
  #       デフォルト 300 = 60 req/min 相当。ビジネス要件に応じて調整すること (#17)。
  # 参考: docs/12_waf_ratelimit_ddos.md §3
}

variable "auth_rate_limit_per_ip" {
  type        = number
  description = "認証エンドポイントへのレートベースルール: 5 分間の IP あたり最大リクエスト数"
  default     = 50
  # 50 = 10 req/min 相当 (ブルートフォース対策) — docs/12_waf_ratelimit_ddos.md §4 参照
}

variable "allowed_ip_cidrs" {
  type        = list(string)
  description = "明示的に許可する IP CIDR リスト (空リストで機能無効)"
  default     = []
  # 例: ["203.0.113.0/24"] — VPN / オフィス IP 等
  # NOTE: 実 IP をコードに書かないこと。tfvars で渡すこと (CLAUDE.local.md)
}

variable "blocked_ip_cidrs" {
  type        = list(string)
  description = "明示的にブロックする IP CIDR リスト (空リストで機能無効)"
  default     = []
}

variable "enable_logging" {
  type        = bool
  description = "WAF ログを CloudWatch Logs に送信するか"
  default     = true
}

variable "log_retention_days" {
  type        = number
  description = "WAF ログの保持日数"
  default     = 90
}

variable "tags" {
  type        = map(string)
  description = "すべてのリソースに付与するタグ"
  default     = {}
}

# ---------------------------------------------------------------------------
# ローカル
# ---------------------------------------------------------------------------

locals {
  name_prefix = "hr-${var.environment}"

  common_tags = merge(
    {
      Environment = var.environment
      ManagedBy   = "terraform"
      Project     = "hr-fullstack"
    },
    var.tags,
  )
}

# ---------------------------------------------------------------------------
# IP セット (許可リスト / 拒否リスト)
# ---------------------------------------------------------------------------

resource "aws_wafv2_ip_set" "allowed" {
  count = length(var.allowed_ip_cidrs) > 0 ? 1 : 0

  name               = "${local.name_prefix}-allowed-ips"
  scope              = "REGIONAL"
  ip_address_version = "IPV4"
  addresses          = var.allowed_ip_cidrs

  tags = local.common_tags
}

resource "aws_wafv2_ip_set" "blocked" {
  count = length(var.blocked_ip_cidrs) > 0 ? 1 : 0

  name               = "${local.name_prefix}-blocked-ips"
  scope              = "REGIONAL"
  ip_address_version = "IPV4"
  addresses          = var.blocked_ip_cidrs

  tags = local.common_tags
}

# ---------------------------------------------------------------------------
# WAFv2 WebACL
# ---------------------------------------------------------------------------

resource "aws_wafv2_web_acl" "main" {
  name  = "${local.name_prefix}-waf"
  scope = "REGIONAL"

  default_action {
    allow {}
  }

  # ─────────────────────────────────────────────
  # Rule 1: 拒否 IP リスト (優先度 0 — 最高優先)
  # ─────────────────────────────────────────────
  dynamic "rule" {
    for_each = length(var.blocked_ip_cidrs) > 0 ? [1] : []
    content {
      name     = "BlocklistedIPs"
      priority = 0

      action {
        block {}
      }

      statement {
        ip_set_reference_statement {
          arn = aws_wafv2_ip_set.blocked[0].arn
        }
      }

      visibility_config {
        cloudwatch_metrics_enabled = true
        metric_name                = "${local.name_prefix}-blocklisted-ips"
        sampled_requests_enabled   = true
      }
    }
  }

  # ─────────────────────────────────────────────
  # Rule 2: 許可 IP リスト (優先度 1 — レートルール等を免除)
  # ─────────────────────────────────────────────
  dynamic "rule" {
    for_each = length(var.allowed_ip_cidrs) > 0 ? [1] : []
    content {
      name     = "AllowlistedIPs"
      priority = 1

      action {
        allow {}
      }

      statement {
        ip_set_reference_statement {
          arn = aws_wafv2_ip_set.allowed[0].arn
        }
      }

      visibility_config {
        cloudwatch_metrics_enabled = true
        metric_name                = "${local.name_prefix}-allowlisted-ips"
        sampled_requests_enabled   = true
      }
    }
  }

  # ─────────────────────────────────────────────
  # Rule 3: AWS Managed Rules — Common Rule Set (OWASP CRS)
  # ─────────────────────────────────────────────
  rule {
    name     = "AWSManagedRulesCommonRuleSet"
    priority = 10

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesCommonRuleSet"
        vendor_name = "AWS"
        # NOTE: 必要に応じて特定ルールを COUNT モードにオーバーライドできる
        # excluded_rule { name = "SizeRestrictions_BODY" }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-aws-common-rules"
      sampled_requests_enabled   = true
    }
  }

  # ─────────────────────────────────────────────
  # Rule 4: AWS Managed Rules — Known Bad Inputs
  # ─────────────────────────────────────────────
  rule {
    name     = "AWSManagedRulesKnownBadInputsRuleSet"
    priority = 20

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesKnownBadInputsRuleSet"
        vendor_name = "AWS"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-known-bad-inputs"
      sampled_requests_enabled   = true
    }
  }

  # ─────────────────────────────────────────────
  # Rule 5: AWS Managed Rules — SQL Injection
  # ─────────────────────────────────────────────
  rule {
    name     = "AWSManagedRulesSQLiRuleSet"
    priority = 30

    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        name        = "AWSManagedRulesSQLiRuleSet"
        vendor_name = "AWS"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-sqli-rules"
      sampled_requests_enabled   = true
    }
  }

  # ─────────────────────────────────────────────
  # Rule 6: レートベースルール — 全 API (#17)
  # ─────────────────────────────────────────────
  rule {
    name     = "RateLimitAllRequests"
    priority = 40

    action {
      block {}
    }

    statement {
      rate_based_statement {
        limit              = var.rate_limit_per_ip
        aggregate_key_type = "IP"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-rate-limit-all"
      sampled_requests_enabled   = true
    }
  }

  # ─────────────────────────────────────────────
  # Rule 7: レートベースルール — 認証エンドポイント (#17, ブルートフォース対策)
  # ─────────────────────────────────────────────
  rule {
    name     = "RateLimitAuthEndpoints"
    priority = 50

    action {
      block {}
    }

    statement {
      rate_based_statement {
        limit              = var.auth_rate_limit_per_ip
        aggregate_key_type = "IP"

        scope_down_statement {
          or_statement {
            statement {
              byte_match_statement {
                field_to_match {
                  uri_path {}
                }
                positional_constraint = "STARTS_WITH"
                search_string         = "/api/v1/auth"
                text_transformation {
                  priority = 0
                  type     = "LOWERCASE"
                }
              }
            }
            statement {
              byte_match_statement {
                field_to_match {
                  uri_path {}
                }
                positional_constraint = "STARTS_WITH"
                search_string         = "/api/v1/login"
                text_transformation {
                  priority = 0
                  type     = "LOWERCASE"
                }
              }
            }
          }
        }
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${local.name_prefix}-rate-limit-auth"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${local.name_prefix}-waf"
    sampled_requests_enabled   = true
  }

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-waf" })
}

# ---------------------------------------------------------------------------
# WebACL を ALB にアタッチ
# ---------------------------------------------------------------------------

resource "aws_wafv2_web_acl_association" "alb" {
  resource_arn = var.alb_arn
  web_acl_arn  = aws_wafv2_web_acl.main.arn
}

# ---------------------------------------------------------------------------
# WAF ログ設定 (CloudWatch Logs)
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "waf" {
  count = var.enable_logging ? 1 : 0

  # WAFv2 ログは "aws-waf-logs-" プレフィックスが必須
  name              = "aws-waf-logs-hr-${var.environment}"
  retention_in_days = var.log_retention_days

  tags = local.common_tags
}

resource "aws_wafv2_web_acl_logging_configuration" "main" {
  count = var.enable_logging ? 1 : 0

  log_destination_configs = [aws_cloudwatch_log_group.waf[0].arn]
  resource_arn            = aws_wafv2_web_acl.main.arn

  # 機密ヘッダーをログから除外
  redacted_fields {
    single_header {
      name = "authorization"
    }
  }

  redacted_fields {
    single_header {
      name = "cookie"
    }
  }
}

# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

output "web_acl_arn" {
  description = "WAFv2 WebACL ARN"
  value       = aws_wafv2_web_acl.main.arn
}

output "web_acl_id" {
  description = "WAFv2 WebACL ID"
  value       = aws_wafv2_web_acl.main.id
}

output "waf_log_group_name" {
  description = "WAF ログ CloudWatch ロググループ名"
  value       = var.enable_logging ? aws_cloudwatch_log_group.waf[0].name : null
}
