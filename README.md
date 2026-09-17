# Jobs · 求职手账 v9.2

一个自托管的求职投递管理工具。当前版本重点加入 **官网进度 7 天循环复查**，并兼容旧版 `orange-v9.1` 的 JSON 数据。

## v9.2 新功能

- 官网投递默认开启状态复查。
- 首次复查时间：`投递日期 + 7 天`。
- 查询后如果仍未 Offer、未淘汰、未结束：自动安排 `本次查询 + 7 天`。
- Offer、明确淘汰、结束或归档后自动停止复查。
- 首页新增「待查官网」统计卡，可一键筛选到期岗位。
- 查询地址优先级：`official_check_url -> source_url -> jd_url`。
- 保存查询次数、最近查询日期、下次查询日期和查询备注。
- schema 9 自动迁移至 schema 10，迁移前会生成原始 JSON 备份。

## 数据安全

真实求职数据不会进入 Git 仓库。以下内容已被 `.gitignore` 排除：

- `data/`
- `jobtracker.json` 及其备份
- SQLite / DB 文件
- `.secret`
- `.env`

应用使用原子写入：先写临时文件并 `fsync`，再通过 `rename` 替换正式 JSON。

## 兼容旧版数据

默认数据文件：

```text
/data/jobtracker.json
```

旧数据示例：

```json
{
  "schema_version": 9,
  "app_version": "9.1",
  "applications": [],
  "events": [],
  "resume_versions": []
}
```

第一次用 v9.2 启动时：

1. 自动复制备份：`jobtracker.json.before-v92-YYYYMMDD-HHMMSS`
2. 官网且非终态的岗位自动补充官网查询字段
3. 首次 `next_official_check_at = applied + 7 days`
4. 更新为 `schema_version = 10`、`app_version = 9.2`

应用记录使用动态 JSON Map 保存，所以旧版本中未被 v9.2 显式使用的字段也不会因为编辑而被删除。

## 官网复查字段

```json
{
  "official_check_enabled": true,
  "official_check_url": "https://career.example.com/candidate",
  "last_official_check_at": "2026-09-17",
  "next_official_check_at": "2026-09-24",
  "official_check_count": 1,
  "official_check_note": "官网仍显示评估中"
}
```

## 本地运行

要求 Go 1.23+：

```bash
go test ./...
go run .
```

默认访问：

```text
http://127.0.0.1:8000
```

可通过环境变量修改：

```bash
JOB_TRACKER_DATA=./data/jobtracker.json \
JOB_TRACKER_ADDR=:8000 \
JOB_TRACKER_TZ=Asia/Shanghai \
go run .
```

## 从旧 v9.1 安全升级

旧数据在宿主机例如：

```text
~/opt/docker/job/data/jobtracker.json
```

先备份：

```bash
cd ~/opt/docker/job
sudo cp -a data "data-backup-$(date +%Y%m%d-%H%M%S)"
```

建议第一次使用副本验证：

```bash
mkdir -p /tmp/jobs-v92
cp ~/opt/docker/job/data/jobtracker.json /tmp/jobs-v92/jobtracker.json

docker run --rm \
  -p 18000:8000 \
  -v /tmp/jobs-v92:/data \
  ghcr.io/zaijianyimian/jobs:latest
```

检查：

```bash
curl http://127.0.0.1:18000/healthz
curl http://127.0.0.1:18000/api/official-checks/due
```

确认数据无误后再替换正式容器。

## Docker Compose

仓库内已提供 `docker-compose.yml`：

```bash
docker compose pull
docker compose up -d
```

如果 GHCR 包尚未公开，需要先登录：

```bash
echo "$GITHUB_TOKEN" | docker login ghcr.io -u zaijianyimian --password-stdin
```

## API

主要接口：

```text
GET    /healthz
GET    /api/applications
POST   /api/applications
GET    /api/applications/{id}
PUT    /api/applications/{id}
DELETE /api/applications/{id}
GET    /api/official-checks/due
POST   /api/applications/{id}/official-check
GET    /api/events
POST   /api/events
POST   /api/events/{id}/toggle
GET    /api/resume-versions
GET    /api/duplicates
GET    /api/analytics/overview
GET    /api/export/gpt?ids=1,2,3
```

## CI/CD

GitHub Actions 工作流位于 `.github/workflows/ci-cd.yml`。

### CI

Pull Request 和 Push 自动执行：

- `gofmt` 检查
- `go vet ./...`
- `go test -race ./...`
- `go build ./...`

### CD

只有 `main` 分支 CI 全部通过后才会：

- 构建 `linux/amd64` 和 `linux/arm64` Docker 镜像
- 登录 GitHub Container Registry
- 推送：
  - `ghcr.io/zaijianyimian/jobs:latest`
  - `ghcr.io/zaijianyimian/jobs:sha-<commit>`

这里的 CD 当前定义为 **持续交付到 GHCR**。如果需要 GitHub Actions 在成功后进一步自动部署到 Fedora 主机，需要额外配置 self-hosted runner 或 SSH 部署凭据，仓库中不会硬编码服务器密钥。

## 端口

Docker Compose 默认：

```text
宿主机 16666 -> 容器 8000
```

## License

Personal project.
