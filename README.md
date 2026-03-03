# DockerProxy

零依赖、单二进制的 Docker Hub 镜像代理，适用于国内网络环境下加速 Docker 镜像拉取。
提供 Web 管理后台、镜像搜索、Manifest/Tag 缓存以及完整的 HTTPS 支持。

---

## 快速开始

### 方式一：直接运行（适合开发/测试）

```bash
# 编译
go build -o dockerproxy ./cmd/dockerproxy

# 启动（默认监听 :8080）
./dockerproxy
```

### 方式三：Releases下载

1.在[Releases](https://github.com/xxmod/DockerProxy/releases)中下载最新版本

2.配置权限

3.使用相应系统的方法打开

4.访问[http://localhost:8080](http://localhost:8080)

---

## 配置 Docker 客户端使用镜像加速

编辑 `/etc/docker/daemon.json`（管理页"Docker 客户端配置"卡片可一键复制）：

```json
{
  "registry-mirrors": [
    "https://proxy.example.com"
  ]
}
```

```bash
sudo systemctl restart docker
```

验证是否生效：

```bash
docker pull nginx
# 观察代理日志中出现 /v2/library/nginx/manifests/... 请求即为成功
```

---

## 环境变量参考

所有配置均通过环境变量设置，启动时读取。

### 基础配置

| 变量                | 说明                                               | 默认值                    |
| ------------------- | -------------------------------------------------- | ------------------------- |
| `LISTEN_ADDR`     | 监听地址                                           | `:8080`                 |
| `PUBLIC_BASE_URL` | 对外访问地址，用于鉴权 realm 改写和管理页命令生成  | `http://localhost:8080` |
| `CONFIG_FILE`     | 配置持久化文件路径，Web 后台保存的配置会写入此文件 | `./data/config.json`    |

### 上游代理

| 变量                    | 说明                             | 默认值                           |
| ----------------------- | -------------------------------- | -------------------------------- |
| `UPSTREAM_REGISTRY`   | 上游 Registry 地址               | `https://registry-1.docker.io` |
| `UPSTREAM_AUTH_REALM` | 上游 Token 鉴权地址              | `https://auth.docker.io/token` |
| `REQUEST_TIMEOUT`     | 上游请求超时（Go duration 格式） | `60s`                          |

### 缓存

| 变量                       | 说明                                                | 默认值              |
| -------------------------- | --------------------------------------------------- | ------------------- |
| `CACHE_DIR`              | 缓存文件存储目录                                    | `./data/cache`    |
| `CACHE_TTL`              | 缓存有效期（Go duration 格式，如 `12h`、`30m`） | `12h`             |
| `CACHE_OBJECT_MAX_BYTES` | 单个缓存对象最大字节数                              | `8388608`（8 MB） |

> 缓存仅针对 manifest 与 tags/list 的 GET 请求，过期由读取时惰性清理。

### HTTPS

| 变量              | 说明                                 | 默认值    |
| ----------------- | ------------------------------------ | --------- |
| `ENABLE_HTTPS`  | 是否启用 HTTPS（`true`/`false`） | `false` |
| `TLS_CERT_FILE` | TLS 证书文件路径                     | ——      |
| `TLS_KEY_FILE`  | TLS 私钥文件路径                     | ——      |
| `HTTP_REDIRECT_ADDR` | HTTP 到 HTTPS 的重定向监听地址 | 自动推导（通常 `:80`） |

> 当 `TLS_CERT_FILE` 和 `TLS_KEY_FILE` 都已配置时，服务会自动启用 HTTPS。
> 启用 HTTPS 后，会启动 HTTP 重定向服务并返回 308 到 HTTPS。若 `:80` 不可用，请显式设置 `HTTP_REDIRECT_ADDR`（例如 `:8081`）。

### 安全与访问控制

| 变量                        | 说明                                       | 默认值                       |
| --------------------------- | ------------------------------------------ | ---------------------------- |
| `ADMIN_TOKEN`             | 管理写操作口令（保存配置、清空缓存时校验） | ——（不设置则写操作无鉴权） |
| `WEB_BASIC_AUTH_USER`     | Web 管理端 Basic Auth 用户名               | ——（不设置则不启用）       |
| `WEB_BASIC_AUTH_PASSWORD` | Web 管理端 Basic Auth 密码                 | ——（不设置则不启用）       |

> `ADMIN_TOKEN` 保护的是管理写操作（PUT / DELETE），通过请求头 `X-Admin-Token` 或 `Authorization: Bearer <token>` 传递。
> Basic Auth 保护的是 Web API（`/api/*`）访问，不影响 `/v2` 与 `/auth/token` 的 Docker 拉取链路。



---



## Web 管理后台

访问 `http://你的地址/` 即可打开管理页面，提供以下功能：

- **镜像搜索**：搜索 Docker Hub 镜像，点击结果一键复制 `docker pull` 命令
- **代理配置**：查看/修改运行时配置，保存后服务自动重启生效
- **Docker 客户端配置**：一键复制 `/etc/docker/daemon.json` 内容
- **运行状态**：查看请求总数、缓存命中/未命中、上游错误等统计
- **缓存管理**：查看缓存条目数、一键清空缓存

---

## API 接口

| 方法   | 路径                       | 说明                       | 鉴权                     |
| ------ | -------------------------- | -------------------------- | ------------------------ |
| GET    | `/healthz`               | 健康检查                   | 无                       |
| GET    | `/api/admin/config`      | 查看当前配置               | Basic Auth（若启用）     |
| PUT    | `/api/admin/config`      | 更新配置（保存后自动重启） | Basic Auth + Admin Token |
| GET    | `/api/admin/stats`       | 查看运行统计               | Basic Auth（若启用）     |
| GET    | `/api/admin/cache`       | 查看缓存条目数             | Basic Auth（若启用）     |
| DELETE | `/api/admin/cache`       | 清空缓存                   | Basic Auth + Admin Token |
| GET    | `/api/search?q=<关键词>` | 搜索 Docker Hub 镜像       | Basic Auth（若启用）     |

---

## 注意事项

- 通过 Web 后台保存的配置会持久化到 `CONFIG_FILE`，重启后自动加载并覆盖环境变量中的对应字段。
- 若对外暴露服务，**强烈建议**同时设置 `ADMIN_TOKEN` 和 `WEB_BASIC_AUTH_USER`/`WEB_BASIC_AUTH_PASSWORD`。
- 缓存目录可挂载独立卷，便于容器重建后保留缓存数据。
