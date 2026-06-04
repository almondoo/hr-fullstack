# modules/container/main.tf
#
# AWS ECS Fargate + ALB + オートスケーリング
# Issue #9 (AWS 確定), #19 (SLA/可用性), #10 (シークレットインジェクション)
#
# 設計方針:
#   - ECS Fargate: サーバーレスコンテナ (OS 管理不要)
#   - ALB: HTTP→HTTPS リダイレクト、/healthz & /readyz ヘルスチェック
#   - マルチ AZ: タスクを複数 AZ に分散 (SLA 99.9% → #19)
#   - オートヒーリング: ALB ヘルスチェック失敗タスクを自動置換
#   - オートスケーリング: CPU/メモリ利用率に基づく水平スケーリング
#   - ローリングデプロイ: ゼロダウンタイムデプロイメント
#   - シークレット: Secrets Manager から実行時インジェクト (#10)

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

variable "region" {
  type        = string
  description = "AWS リージョン"
}

variable "vpc_id" {
  type        = string
  description = "デプロイ先 VPC ID"
}

variable "public_subnet_ids" {
  type        = list(string)
  description = "ALB を配置するパブリックサブネット ID リスト"
}

variable "private_subnet_ids" {
  type        = list(string)
  description = "ECS タスクを配置するプライベートサブネット ID リスト"
}

variable "alb_security_group_id" {
  type        = string
  description = "ALB セキュリティグループ ID"
}

variable "app_security_group_id" {
  type        = string
  description = "ECS タスクセキュリティグループ ID"
}

variable "image_repository_url" {
  type        = string
  description = "ECR リポジトリ URL (例: 123456789.dkr.ecr.ap-northeast-1.amazonaws.com/hr-api)"
  # NOTE: アカウント ID は .tfvars で渡す。コードに書かない (CLAUDE.local.md)
}

variable "image_tag" {
  type        = string
  description = "コンテナイメージタグ (コミット SHA 等)"
  default     = "latest"
  # NOTE: 本番では latest タグを使わないこと。CI/CD からコミット SHA を渡すこと。
}

variable "app_port" {
  type        = number
  description = "アプリケーションコンテナのポート番号"
  default     = 8080
}

variable "cpu" {
  type        = number
  description = "Fargate タスク CPU ユニット (256/512/1024/2048/4096)"
  default     = 256
}

variable "memory" {
  type        = number
  description = "Fargate タスクメモリ (MiB)"
  default     = 512
}

variable "min_instances" {
  type        = number
  description = "最小タスク数 (SLA 99.9% のため本番は最低 2 → #19)"
  default     = 2
}

variable "max_instances" {
  type        = number
  description = "最大タスク数 (オートスケーリング上限)"
  default     = 10
}

variable "scale_up_cpu_threshold" {
  type        = number
  description = "スケールアップ CPU 使用率閾値 (%)"
  default     = 70
}

variable "scale_down_cpu_threshold" {
  type        = number
  description = "スケールダウン CPU 使用率閾値 (%)"
  default     = 30
}

variable "secrets_manager_arns" {
  type        = list(string)
  description = "タスクからアクセスを許可する Secrets Manager ARN リスト (#10)"
  default     = []
}

variable "kms_key_arns" {
  type        = list(string)
  description = "タスクからアクセスを許可する KMS キー ARN リスト (#10)"
  default     = []
}

variable "certificate_arn" {
  type        = string
  description = "ALB HTTPS リスナーに使用する ACM 証明書 ARN"
  default     = null
  # null の場合は HTTPS リスナーを作成しない (開発環境向け)
}

variable "health_check_grace_period_seconds" {
  type        = number
  description = "ECS サービス起動後のヘルスチェック猶予期間 (秒)"
  default     = 60
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
# ECR リポジトリ
# ---------------------------------------------------------------------------

resource "aws_ecr_repository" "app" {
  name                 = "${local.name_prefix}-api"
  image_tag_mutability = "IMMUTABLE"

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-api" })
}

resource "aws_ecr_lifecycle_policy" "app" {
  repository = aws_ecr_repository.app.name

  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep last 30 images"
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 30
      }
      action = { type = "expire" }
    }]
  })
}

# ---------------------------------------------------------------------------
# ECS クラスター
# ---------------------------------------------------------------------------

resource "aws_ecs_cluster" "main" {
  name = "${local.name_prefix}-cluster"

  setting {
    name  = "containerInsights"
    value = "enabled"
  }

  tags = local.common_tags
}

resource "aws_ecs_cluster_capacity_providers" "main" {
  cluster_name       = aws_ecs_cluster.main.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    weight            = 1
    base              = var.min_instances
  }
}

# ---------------------------------------------------------------------------
# IAM ロール — タスク実行ロール (ECR pull + Secrets Manager 読み取り)
# ---------------------------------------------------------------------------

resource "aws_iam_role" "task_execution" {
  name = "${local.name_prefix}-ecs-execution-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "task_execution_managed" {
  role       = aws_iam_role.task_execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# タスク実行ロールに Secrets Manager アクセスを追加 (#10)
resource "aws_iam_role_policy" "task_execution_secrets" {
  count = length(var.secrets_manager_arns) > 0 ? 1 : 0

  name = "${local.name_prefix}-ecs-execution-secrets"
  role = aws_iam_role.task_execution.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.secrets_manager_arns
      },
      {
        Effect   = "Allow"
        Action   = ["kms:Decrypt"]
        Resource = length(var.kms_key_arns) > 0 ? var.kms_key_arns : ["*"]
        Condition = {
          StringEquals = {
            "kms:ViaService" = "secretsmanager.${var.region}.amazonaws.com"
          }
        }
      },
    ]
  })
}

# ---------------------------------------------------------------------------
# IAM ロール — タスクロール (アプリが実行時に使用するロール — 最小権限)
# ---------------------------------------------------------------------------

resource "aws_iam_role" "task" {
  name = "${local.name_prefix}-ecs-task-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action    = "sts:AssumeRole"
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
    }]
  })

  tags = local.common_tags
}

# タスクロールから Secrets Manager を読み取る権限 (#10)
resource "aws_iam_role_policy" "task_secrets" {
  count = length(var.secrets_manager_arns) > 0 ? 1 : 0

  name = "${local.name_prefix}-ecs-task-secrets"
  role = aws_iam_role.task.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["secretsmanager:GetSecretValue"]
        Resource = var.secrets_manager_arns
      },
      {
        Effect   = "Allow"
        Action   = ["kms:Decrypt", "kms:GenerateDataKey"]
        Resource = length(var.kms_key_arns) > 0 ? var.kms_key_arns : ["*"]
      },
    ]
  })
}

# ---------------------------------------------------------------------------
# CloudWatch ロググループ
# ---------------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "app" {
  name              = "/ecs/${local.name_prefix}"
  retention_in_days = 90

  tags = local.common_tags
}

# ---------------------------------------------------------------------------
# ECS タスク定義
# ---------------------------------------------------------------------------

resource "aws_ecs_task_definition" "app" {
  family                   = "${local.name_prefix}-api"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = var.cpu
  memory                   = var.memory
  execution_role_arn       = aws_iam_role.task_execution.arn
  task_role_arn            = aws_iam_role.task.arn

  container_definitions = jsonencode([
    {
      name      = "api"
      image     = "${var.image_repository_url}:${var.image_tag}"
      essential = true

      portMappings = [{
        containerPort = var.app_port
        protocol      = "tcp"
      }]

      # シークレットを環境変数としてインジェクト (#10)
      # 実際のシークレット ARN は environments/ 側の tfvars で渡す
      secrets = []
      # 例:
      # secrets = [
      #   { name = "FIELD_ENCRYPTION_KEY", valueFrom = "<secret-arn>:FIELD_ENCRYPTION_KEY::" },
      #   { name = "DATABASE_URL",         valueFrom = "<secret-arn>:DATABASE_URL::" },
      #   { name = "SESSION_KEY",          valueFrom = "<secret-arn>:SESSION_KEY::" },
      #   { name = "CSRF_KEY",             valueFrom = "<secret-arn>:CSRF_KEY::" },
      # ]

      environment = [
        { name = "ENV", value = var.environment },
        { name = "PORT", value = tostring(var.app_port) },
      ]

      # ヘルスチェック: /healthz (Liveness) → #19
      healthCheck = {
        command     = ["CMD-SHELL", "wget -qO- http://localhost:${var.app_port}/healthz || exit 1"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 60
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.app.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "ecs"
        }
      }

      readonlyRootFilesystem = true
      # NOTE: コンテナ内書き込みが必要な場合は mountPoints で tmpfs を追加すること
    }
  ])

  tags = local.common_tags
}

# ---------------------------------------------------------------------------
# ALB (Application Load Balancer)
# ---------------------------------------------------------------------------

resource "aws_lb" "main" {
  name               = "${local.name_prefix}-alb"
  internal           = false
  load_balancer_type = "application"
  security_groups    = [var.alb_security_group_id]
  subnets            = var.public_subnet_ids

  enable_deletion_protection = var.environment == "production"

  # アクセスログ (セキュリティ監査用 — S3 バケットは別途設定推奨)
  # access_logs {
  #   bucket  = "<your-alb-logs-bucket>"
  #   prefix  = "hr-${var.environment}-alb"
  #   enabled = true
  # }

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-alb" })
}

# ---------------------------------------------------------------------------
# ALB ターゲットグループ (/readyz でヘルスチェック → #19)
# ---------------------------------------------------------------------------

resource "aws_lb_target_group" "app" {
  name        = "${local.name_prefix}-tg"
  port        = var.app_port
  protocol    = "HTTP"
  vpc_id      = var.vpc_id
  target_type = "ip" # Fargate は ip ターゲット

  health_check {
    enabled             = true
    path                = "/readyz"
    port                = "traffic-port"
    protocol            = "HTTP"
    healthy_threshold   = 2
    unhealthy_threshold = 3
    timeout             = 5
    interval            = 30
    matcher             = "200"
  }

  deregistration_delay = 30

  tags = merge(local.common_tags, { Name = "${local.name_prefix}-tg" })
}

# ---------------------------------------------------------------------------
# ALB リスナー
# ---------------------------------------------------------------------------

# HTTP → HTTPS リダイレクト
resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"
    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}

# HTTPS リスナー (certificate_arn が設定された場合)
resource "aws_lb_listener" "https" {
  count = var.certificate_arn != null ? 1 : 0

  load_balancer_arn = aws_lb.main.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }
}

# ---------------------------------------------------------------------------
# ECS サービス
# ---------------------------------------------------------------------------

resource "aws_ecs_service" "app" {
  name                              = "${local.name_prefix}-api"
  cluster                           = aws_ecs_cluster.main.id
  task_definition                   = aws_ecs_task_definition.app.arn
  desired_count                     = var.min_instances
  launch_type                       = "FARGATE"
  health_check_grace_period_seconds = var.health_check_grace_period_seconds
  enable_execute_command            = false # 本番では ECS Exec を無効化

  # ローリングデプロイ設定 (#19 ゼロダウンタイム)
  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  deployment_circuit_breaker {
    enable   = true
    rollback = true # デプロイ失敗時に自動ロールバック
  }

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [var.app_security_group_id]
    assign_public_ip = false
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = "api"
    container_port   = var.app_port
  }

  # NOTE: Fargate では placement_constraints は使えない。
  #       複数のプライベートサブネット (AZ 分散) を private_subnet_ids に渡すことで
  #       複数 AZ への分散配置を実現する (#19)。

  tags = local.common_tags

  lifecycle {
    ignore_changes = [desired_count, task_definition]
    # desired_count は Auto Scaling が管理
    # task_definition は CI/CD が更新
  }

  depends_on = [aws_lb_listener.http]
}

# ---------------------------------------------------------------------------
# オートスケーリング (#19)
# ---------------------------------------------------------------------------

resource "aws_appautoscaling_target" "ecs" {
  max_capacity       = var.max_instances
  min_capacity       = var.min_instances
  resource_id        = "service/${aws_ecs_cluster.main.name}/${aws_ecs_service.app.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  service_namespace  = "ecs"
}

# CPU 使用率によるスケーリング
resource "aws_appautoscaling_policy" "cpu" {
  name               = "${local.name_prefix}-cpu-scaling"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.ecs.resource_id
  scalable_dimension = aws_appautoscaling_target.ecs.scalable_dimension
  service_namespace  = aws_appautoscaling_target.ecs.service_namespace

  target_tracking_scaling_policy_configuration {
    target_value       = var.scale_up_cpu_threshold
    scale_in_cooldown  = 300
    scale_out_cooldown = 60

    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
  }
}

# メモリ使用率によるスケーリング
resource "aws_appautoscaling_policy" "memory" {
  name               = "${local.name_prefix}-memory-scaling"
  policy_type        = "TargetTrackingScaling"
  resource_id        = aws_appautoscaling_target.ecs.resource_id
  scalable_dimension = aws_appautoscaling_target.ecs.scalable_dimension
  service_namespace  = aws_appautoscaling_target.ecs.service_namespace

  target_tracking_scaling_policy_configuration {
    target_value       = 80.0
    scale_in_cooldown  = 300
    scale_out_cooldown = 60

    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageMemoryUtilization"
    }
  }
}

# ---------------------------------------------------------------------------
# 出力
# ---------------------------------------------------------------------------

output "alb_dns_name" {
  description = "ALB の DNS 名 (Route 53 エイリアスレコードのターゲット)"
  value       = aws_lb.main.dns_name
}

output "alb_zone_id" {
  description = "ALB の ホストゾーン ID (Route 53 エイリアスレコード用)"
  value       = aws_lb.main.zone_id
}

output "ecs_cluster_name" {
  description = "ECS クラスター名"
  value       = aws_ecs_cluster.main.name
}

output "ecs_service_name" {
  description = "ECS サービス名"
  value       = aws_ecs_service.app.name
}

output "task_role_arn" {
  description = "ECS タスクロール ARN (secrets モジュールに渡す)"
  value       = aws_iam_role.task.arn
}

output "task_execution_role_arn" {
  description = "ECS タスク実行ロール ARN"
  value       = aws_iam_role.task_execution.arn
}

output "ecr_repository_url" {
  description = "ECR リポジトリ URL"
  value       = aws_ecr_repository.app.repository_url
}

output "alb_arn" {
  description = "ALB ARN (WAF モジュールに渡す)"
  value       = aws_lb.main.arn
}

output "service_url" {
  description = "サービス URL (ALB DNS ベース — 本番は Route 53 カスタムドメインを使用)"
  value       = "https://${aws_lb.main.dns_name}"
}
