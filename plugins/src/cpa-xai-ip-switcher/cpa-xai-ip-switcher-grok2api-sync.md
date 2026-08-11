# cpa-xai-ip-switcher × grok2api 代理槽同步接入说明

> 给 **CPA 插件 `cpa-xai-ip-switcher` 的开发 agent** 用。  
> 目标：在插件配置界面提供 grok2api 连接信息，并在 IP/账号槽位变化时，把代理与账号绑定同步到远端 grok2api。

---

## 1. 目标行为

插件维护若干 **slot（0–50）**，每个 slot 有：

- 代理地址 `ip`（可为 `http` / `https` / `socks5`，可带用户名密码；也可为空）
- 绑定的账号邮箱列表 `accounts`（Outlook 等）

每次需要同步时，调用 grok2api 的 **CPA Auto Proxy Slots API**。远端会：

| 条件 | grok2api 行为 |
|------|----------------|
| `ip` 非空 | 创建或更新名为 `cpa_auto_proxy_{slot}` 的出口节点 |
| `ip` 为空 | 删除 `cpa_auto_proxy_{slot}`（不存在则视为已清除） |
| 节点属性 | 已启用、作用域 `grok_console`、账号容量无限制（0）、`proxyPool=false`、代理地址=传入 `ip` |
| `accounts` | 按 **email** 在 Grok Console 号池中查找并 **手工绑定** 到该节点；不存在则跳过 |

**重要语义：**

- 只绑定本次传入的账号；**不会**自动解绑该节点上其它旧账号。
- 同 email 若有多条 Console 账号，会全部绑定。
- 同一次请求中 **slot 不可重复**。
- slot 合法范围：**0 ≤ slot ≤ 50**。

---

## 2. 插件配置界面（必须实现）

在 `cpa-xai-ip-switcher` 配置页增加三个输入项（持久化到插件配置）：

| 配置项（UI 文案） | 建议 config key | 类型 | 说明 |
|-------------------|-----------------|------|------|
| grok2api 远端地址 | `grok2apiBaseUrl` | string | 例：`https://g2.example.com` 或 `http://127.0.0.1:8000`。**不要**带尾斜杠；**不要**带 `/api/admin/v1` 路径。 |
| grok2api 管理员用户名 | `grok2apiAdminUsername` | string | 管理后台登录用户名 |
| grok2api 管理员密码 | `grok2apiAdminPassword` | string | 管理后台登录密码；按密钥字段存储（勿打日志） |

可选（非必须，可后加）：

| 配置项 | 说明 |
|--------|------|
| 启用同步开关 | 关闭时只改本地 slot，不调远端 |
| 连接测试按钮 | 调 login + 可选空 payload 校验，提示成功/失败 |

**校验建议：**

- `grok2apiBaseUrl` 必须为绝对 URL（`http://` 或 `https://`）。
- 用户名、密码非空才允许发起同步。
- 保存时 `trim` baseUrl 尾部 `/`。

---

## 3. 鉴权流程（Admin JWT）

所有管理 API 前缀：`{baseUrl}/api/admin/v1`

### 3.1 登录

```
POST {baseUrl}/api/admin/v1/auth/login
Content-Type: application/json
```

Body:

```json
{
  "username": "<管理员用户名>",
  "password": "<管理员密码>"
}
```

成功 `200`：

```json
{
  "data": {
    "admin": {
      "id": "1",
      "username": "admin"
    },
    "tokens": {
      "accessToken": "<JWT>",
      "accessTokenExpiresAt": "2026-08-11T12:00:00Z",
      "refreshTokenExpiresAt": "2026-09-10T12:00:00Z"
    }
  }
}
```

失败示例：

| HTTP | code | 含义 |
|------|------|------|
| 401 | `invalidCredentials` | 用户名或密码错误 |
| 429 | `loginRateLimited` | 登录过频 |
| 503 | `authRuntimeUnavailable` | 认证服务不可用 |

**插件侧推荐策略（机器调用）：**

1. 每次同步前 login，取 `data.tokens.accessToken`。
2. 或缓存 accessToken，收到 **401 `adminUnauthorized`** 时重新 login 再重试 **一次**。
3. **不要依赖 refresh cookie**。  
   - refresh token **不会**出现在 JSON body（只写 HttpOnly cookie）。  
   - cookie `Path=/api/admin/v1/auth`，`SameSite=Strict`，跨机/插件 HTTP 客户端很难用。  
   - 重新 login 最简单、最稳。

### 3.2 调用受保护 API

```
Authorization: Bearer <accessToken>
```

无 cookie 不能替代 Bearer。

---

## 4. 同步 API（核心）

```
POST {baseUrl}/api/admin/v1/cpa-auto-proxy/slots
Authorization: Bearer <accessToken>
Content-Type: application/json
```

### 4.1 Request body

**顶层是数组**（不是 `{ slots: [...] }`）。

```json
[
  {
    "slot": 0,
    "ip": "http://username:pass@1.2.3.4:8080",
    "accounts": ["xxx@outlook.com", "yyy@outlook.com"]
  },
  {
    "slot": 1,
    "ip": "",
    "accounts": ["xxx1@outlook.com"]
  }
]
```

| 字段 | 类型 | 必填 | 规则 |
|------|------|------|------|
| `slot` | int | 是 | 0–50，同请求唯一 |
| `ip` | string | 是（可空串） | 非空：合法代理 URL；空：删除节点 |
| `accounts` | string[] | 是（可 `[]`） | 每项 trim 后不可为空串；不存在于 Console 则跳过 |

**代理 URL 格式（与 grok2api 校验一致）：**

- scheme：`http` / `https` / `socks4` / `socks4a` / `socks5` / `socks5h`
- 形如：`scheme://[user:pass@]host:port`
- **禁止** path、query、fragment
- 示例：
  - `http://user:pass@10.0.0.1:3128`
  - `https://user:pass@10.0.0.1:443`
  - `socks5://user:pass@10.0.0.1:1080`

JSON 字段名必须是 **`ip`**（不是 `ip:`）。

### 4.2 成功响应 `200`

统一 Admin 信封：`{ "data": ... }`

```json
{
  "data": {
    "results": [
      {
        "slot": 0,
        "action": "created",
        "nodeName": "cpa_auto_proxy_0",
        "nodeId": "12",
        "assigned": 2,
        "skippedAccounts": []
      },
      {
        "slot": 1,
        "action": "deleted",
        "nodeName": "cpa_auto_proxy_1",
        "nodeId": "13",
        "assigned": 0,
        "skippedAccounts": ["xxx1@outlook.com"]
      }
    ]
  }
}
```

| `action` | 含义 |
|----------|------|
| `created` | 新建节点并尝试绑定 |
| `updated` | 更新已有节点并尝试绑定 |
| `deleted` | 因 `ip` 为空删除节点 |
| `absent` | `ip` 为空且节点本就不存在 |
| `failed` | 该 slot 处理失败；看 `error` |

| 字段 | 含义 |
|------|------|
| `nodeName` | 固定 `cpa_auto_proxy_{slot}` |
| `nodeId` | 节点 ID（字符串）；删除/缺席可能无 |
| `assigned` | 实际绑定成功的账号数 |
| `skippedAccounts` | Console 中找不到的 email |
| `error` | 仅 `failed` 时有值 |

**注意：** 顶层 HTTP 200 不代表每个 slot 都成功。插件必须逐项检查 `results[].action` / `error`。

### 4.3 请求级错误（整包失败）

| HTTP | code | 场景 |
|------|------|------|
| 400 | `invalidRequest` | JSON 非数组、空数组、slot 越界/重复、accounts 含空串 |
| 401 | `adminUnauthorized` | token 无效/过期 |
| 500 | `cpaAutoProxyAccountLookupFailed` | 读 Console 账号失败 |
| 500 | `cpaAutoProxyNodeLookupFailed` | 读出口节点失败 |

错误体：

```json
{
  "error": {
    "code": "invalidRequest",
    "message": "...",
    "requestId": "..."
  }
}
```

---

## 5. 插件侧推荐实现

### 5.1 何时触发同步

任选或组合（按产品需求）：

1. **槽位变更后自动同步**（IP 切换、账号列表变更）
2. **手动「立即同步」按钮**
3. **定时全量同步**（例如每 N 分钟推一次当前全部 slot）

### 5.2 同步伪代码

```text
function syncSlotsToGrok2api(slots):
  baseUrl = trimRight(config.grok2apiBaseUrl, "/")
  if baseUrl empty or username/password empty:
    skip or warn "未配置 grok2api"
    return

  // 1) login
  loginResp = POST baseUrl + "/api/admin/v1/auth/login"
    body { username, password }
  if not 200:
    report auth failure (code/message)
    return
  accessToken = loginResp.data.tokens.accessToken

  // 2) build payload from plugin local state
  payload = []
  for each local slot in 0..50 that should be reported:
    payload.append({
      slot: slotIndex,
      ip: proxyUrlOrEmpty,          // empty string deletes remote node
      accounts: list of emails      // may be []
    })
  if payload empty:
    return

  // 3) sync
  syncResp = POST baseUrl + "/api/admin/v1/cpa-auto-proxy/slots"
    header Authorization: "Bearer " + accessToken
    body payload   // raw JSON array

  if 401:
    // optional: re-login once and retry
    ...
  if not 200:
    report error(syncResp.error)
    return

  for each item in syncResp.data.results:
    if item.action == "failed":
      log slot failure item.error
    if item.skippedAccounts not empty:
      log/warn missing console accounts
```

### 5.3 全量 vs 增量

| 模式 | 做法 | 建议 |
|------|------|------|
| 全量 | 每次把插件关心的全部 slot（0–N）推上去 | 实现简单；远端与 CPA 一致 |
| 增量 | 只推变更的 slot | 流量小；注意删除要用 `ip:""` 显式删 |

**删除节点必须显式传 `ip: ""`。** 不传该 slot 不会自动删除远端节点。

### 5.4 安全

- 密码、代理 URL 中的凭据：**禁止写入普通日志**。
- HTTPS 优先；HTTP 仅内网调试。
- accessToken 仅内存短时缓存，勿落盘明文。

### 5.5 超时与重试

- login / sync 建议超时 **15–30s**。
- 仅对网络错误、5xx 有限重试（如 1–2 次）；**4xx 不重试**（401 除外可 re-login 一次）。
- 429 login：退避后提示用户。

---

## 6. curl 自测样例

把 `BASE`、用户、密码换成真实值。

```bash
# 1) 登录
LOGIN=$(curl -sS -X POST "$BASE/api/admin/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"your-password"}')

echo "$LOGIN"
TOKEN=$(echo "$LOGIN" | jq -r '.data.tokens.accessToken')

# 2) 同步两个 slot
curl -sS -X POST "$BASE/api/admin/v1/cpa-auto-proxy/slots" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '[
    {
      "slot": 0,
      "ip": "socks5://user:pass@1.2.3.4:1080",
      "accounts": ["a@outlook.com", "b@outlook.com"]
    },
    {
      "slot": 1,
      "ip": "",
      "accounts": []
    }
  ]' | jq .
```

---

## 7. 远端节点命名约定

| slot | 节点名称 |
|------|----------|
| 0 | `cpa_auto_proxy_0` |
| 1 | `cpa_auto_proxy_1` |
| … | … |
| 50 | `cpa_auto_proxy_50` |

作用域固定 **Grok Console**（`grok_console`）。  
账号必须是 grok2api 里 **已导入的 Grok Console 账号** 且 **email 字段匹配**（大小写不敏感）。仅有 Web/Build、没有 Console 对应账号的 email 会出现在 `skippedAccounts`。

---

## 8. UI 文案建议（中文）

```
【grok2api 同步】
远端地址：     [ https://your-g2-host          ]
管理员用户名： [ admin                         ]
管理员密码：   [ ********                      ]

说明：IP 切换或账号变更后，将同步到 grok2api 的
cpa_auto_proxy_0 ~ cpa_auto_proxy_50 出口节点，
并绑定对应 Grok Console 账号。
```

错误提示映射：

| 情况 | 提示 |
|------|------|
| 未配置 baseUrl/用户/密码 | 请先填写 grok2api 连接信息 |
| 401 / invalidCredentials | 管理员用户名或密码错误 |
| 429 | 登录过于频繁，请稍后重试 |
| skippedAccounts 非空 | 以下邮箱在 Grok Console 中不存在，已跳过：… |
| action=failed | 槽位 N 同步失败：… |

---

## 9. 实现清单（给 agent 勾选）

- [ ] 配置页三字段：`grok2apiBaseUrl` / `grok2apiAdminUsername` / `grok2apiAdminPassword`
- [ ] 配置持久化；密码不明文日志
- [ ] baseUrl 规范化（去尾 `/`）
- [ ] `POST /api/admin/v1/auth/login` 取 `accessToken`
- [ ] `POST /api/admin/v1/cpa-auto-proxy/slots`，Bearer 鉴权，body 为 **数组**
- [ ] 本地 slot → `{ slot, ip, accounts }` 映射
- [ ] `ip` 空串表示删除远端节点
- [ ] 解析 `data.results`，处理 `failed` / `skippedAccounts`
- [ ] 401 时 re-login 重试一次
- [ ]（可选）手动同步按钮 + 简单连接测试

---

## 10. 与上游合并说明（背景）

远端实现位于 grok2api 本地扩展包：

- `backend/internal/transport/http/cpaautopproxy/handler.go`
- 路由挂载：`POST /api/admin/v1/cpa-auto-proxy/slots`（Admin 鉴权组）

CPA 插件只需 HTTP 客户端，不依赖 grok2api 源码。

---

## 11. 最小验收标准

1. 填对 baseUrl + 管理员账号密码。  
2. slot0 设 `socks5://...` + 两个已存在 Console email → 管理后台出现 `cpa_auto_proxy_0`，账号已绑定。  
3. 同一 slot 改 `ip` 再同步 → 节点更新，非新建重复名策略由远端按名 update。  
4. slot0 的 `ip` 改为 `""` 再同步 → 节点删除。  
5. 传入不存在的 email → 该 email 出现在 `skippedAccounts`，其它正常。  
6. 错误密码 → login 失败，有明确 UI 错误，不同步。
