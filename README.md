# DockerProxy

一个使用 Go 实现的 Docker 镜像代理工具，支持：

- 使用单一域名代理 Docker Hub 镜像拉取（适合作为 Docker mirror）
- `/auth/token` 鉴权中继与 `WWW-Authenticate` realm 自动改写
- Manifest/Tag 请求本地文件缓存（减少重复请求）
- Web 管理后台（配置查看/更新、统计、缓存清理）
- 镜像搜索（Docker Hub）

## 1. 启动

```bash
go run ./cmd/dockerproxy
```

默认监听：`0.0.0.0:8080`

## 2. 环境变量

- `LISTEN_ADDR`：监听地址，默认 `:8080`
- `CONFIG_FILE`：后台配置持久化文件路径，默认 `./data/config.json`
- `ENABLE_HTTPS`：是否启用 HTTPS（`true/false`），默认 `false`
- `TLS_CERT_FILE`：TLS 证书文件路径（启用 HTTPS 时必填）
- `TLS_KEY_FILE`：TLS 私钥文件路径（启用 HTTPS 时必填）
  - 当 `TLS_CERT_FILE` 和 `TLS_KEY_FILE` 都已配置时，服务会自动切换为 HTTPS 模式
- `PUBLIC_BASE_URL`：外部访问地址（用于鉴权 realm 改写），默认 `http://localhost:8080`
- `UPSTREAM_REGISTRY`：上游 registry，默认 `https://registry-1.docker.io`
- `UPSTREAM_AUTH_REALM`：上游 token 地址，默认 `https://auth.docker.io/token`
- `CACHE_DIR`：缓存目录，默认 `./data/cache`
- `CACHE_TTL`：缓存时长，默认 `12h`
- `CACHE_OBJECT_MAX_BYTES`：单对象缓存最大字节，默认 `8388608`（8MB）
- `REQUEST_TIMEOUT`：上游请求超时，默认 `60s`
- `ADMIN_TOKEN`：管理写操作口令（可选）
- `WEB_BASIC_AUTH_USER`：Web 管理端 Basic Auth 用户名（可选）
- `WEB_BASIC_AUTH_PASSWORD`：Web 管理端 Basic Auth 密码（可选）

## 3. Docker 客户端配置镜像加速

编辑 Docker daemon 配置（例如 Linux 的 `/etc/docker/daemon.json`）：

```json
{
  "registry-mirrors": [
    "https://你的代理域名"
  ]
}
```

然后重启 Docker 服务。

## 4. 管理接口

- `GET /api/admin/config`：查看配置
- `PUT /api/admin/config`：更新配置（支持 `public_base_url`、`upstream_registry`、`upstream_auth_realm`）
  - 也支持 HTTPS 相关配置：`enable_https`、`tls_cert_file`、`tls_key_file`
- `GET /api/admin/stats`：查看统计
- `GET /api/admin/cache`：查看缓存文件数
- `DELETE /api/admin/cache`：清空缓存
- `GET /api/search?q=nginx`：搜索镜像

Web 管理页面：`/`

## 5. 注意事项

- 当前缓存策略只针对 `manifest` 与 `tags/list` 相关 GET 请求。
- HTTPS 配置更新后需要重启服务才能生效（监听模式在启动时决定）。
- 通过后台 `PUT /api/admin/config` 保存的配置会写入 `CONFIG_FILE`，服务重启后会自动加载。
- 若对外暴露管理接口，建议务必设置 `ADMIN_TOKEN`，并通过反向代理加上 HTTPS 与访问控制。
