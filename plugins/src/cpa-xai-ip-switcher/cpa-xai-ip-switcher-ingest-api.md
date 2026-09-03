# cpa-xai-ip-switcher 外部录入接口

给外部系统用。提交一批代理 IP，走插件现有「录入并探测」链路。  
不走 WebUI 包装接口，也不录入为【健康保底】。

插件版本：`0.1.7`

---

## 1. 接口

```
POST /v0/management/cpa-xai-ip-switcher/nodes
```

挂在 CPA 管理面。插件自身不再做第二套鉴权。

| 项 | 值 |
|----|----|
| Method | `POST` |
| Path | `/v0/management/cpa-xai-ip-switcher/nodes` |
| Content-Type | `application/json`（也接受纯文本） |
| 成功 | `200` |
| 探测 | 异步。接口只负责入库；已启动的探测线程随后领取【未探测】节点 |

不要调用：

```
POST /v0/management/cpa-xai-ip-switcher/api
```

那是 WebUI 专用包装口，需要 `X-CPA-XAI-IP-Switcher-UI: 1` 和 `{method,path,body}` 内层请求。外部调用会 `403`。

---

## 2. 鉴权

CPA 管理密钥，二选一：

```
Authorization: Bearer <management-key>
```

```
X-Management-Key: <management-key>
```

`Authorization` 也可不带 `Bearer` 前缀，整段当密钥。

约束：

- 本机 `127.0.0.1` / `::1`：用管理密钥即可。
- 非本机：CPA 必须开启远程管理（`allow-remote-management`），否则 `403 remote management disabled`。
- 未配置管理密钥：`403 remote management key not set`。
- 缺密钥：`401 missing management key`。
- 同一客户端连续失败 5 次：封禁 30 分钟。

密钥来源是 CPA 主程序管理密钥，不是插件 SQLite 配置，也不是 `api-keys`。

---

## 3. 请求体

解析优先级：JSON 对象字段 → JSON 数组 → JSON 字符串 → 原始文本。  
最终都拆成按行录入。空行忽略。

对象字段按以下顺序取**第一个非空**：

`text`、`ips`、`ip`、`proxy`、`proxyUrl`、`proxy_url`、`url`、`line`、`nodes`、`proxies`、`lines`、`items`

字段值可以是：

- 字符串（内部再按换行拆）
- 字符串数组
- 对象数组（再按上面字段名取代理）

`manualFallback` **无效**。本接口固定走初次探测。

### 3.1 推荐：对象 + 数组

```json
{
  "ips": [
    "socks5://user:pass@38.244.38.76:8888",
    "http://user:pass@1.2.3.4:8080"
  ]
}
```

### 3.2 对象 + 多行字符串

```json
{
  "text": "socks5://user:pass@38.244.38.76:8888\nhttp://user:pass@1.2.3.4:8080"
}
```

### 3.3 JSON 数组

```json
[
  "socks5://user:pass@38.244.38.76:8888",
  "http://user:pass@1.2.3.4:8080"
]
```

### 3.4 纯文本（非 JSON）

Body 不是 `{` / `[` / `"` 开头时，整段当多行文本：

```
socks5://user:pass@38.244.38.76:8888
http://user:pass@1.2.3.4:8080
```

---

## 4. 每行格式

与 WebUI 录入相同。一行一个节点。

### 4.1 代理 URL

```
http://[user:pass@]host:port
https://[user:pass@]host:port
socks5://[user:pass@]host:port
socks5h://[user:pass@]host:port
```

必须有 scheme、host、port。不要路径、查询、fragment。

### 4.2 CSV

```
host:port,ip,port,protocol[,domain]
```

例：

```
38.244.38.76:8888,38.244.38.76,8888,socks5
```

`protocol` 只能是 `http`、`https`、`socks5`、`socks5h`。

格式错误的行不入库，进入响应 `errors`。

---

## 5. 行为

1. 解析出行。
2. `INSERT OR IGNORE` 写入 `ip_nodes`。`proxy_url` 已存在算重复，不覆盖。
3. 新节点状态为【未探测】，记录 `entered_at`。
4. 只要新增 `added > 0`，创建批次并开始初次探测（连通性）。
5. 接口立即返回，不等探测结束。

重复判定键是完整 `proxy_url`，不是裸 IP。

---

## 6. 响应

`Content-Type: application/json; charset=utf-8`

成功 `200`：

```json
{
  "data": {
    "batchId": "B1730000000000000000",
    "added": 2,
    "duplicates": 1,
    "errors": [
      {
        "line": 4,
        "message": "协议必须是 http、https、socks5 或 socks5h"
      }
    ]
  }
}
```

| 字段 | 含义 |
|------|------|
| `batchId` | 本次录入批次 |
| `added` | 新写入节点数 |
| `duplicates` | `proxy_url` 已存在、未写入 |
| `errors` | 行级格式错误。行号从 1 起。空行不占错误 |

无任何有效新节点、也无重复、只有格式错误时：`400`

```json
{
  "error": {
    "code": "invalidInput",
    "message": "没有可录入的有效 IP"
  },
  "data": {
    "batchId": "B...",
    "added": 0,
    "duplicates": 0,
    "errors": [{"line": 1, "message": "..."}]
  }
}
```

其它错误：

| HTTP | code | 场景 |
|------|------|------|
| 401 / 403 | （宿主 JSON `error` 字符串） | 管理密钥缺失、错误、远程管理关闭、IP 封禁 |
| 400 | `invalidBody` | 空 body、JSON 无法解析、解析后无 IP |
| 400 | `emptyInput` | 规范化后文本为空（内部录入链路） |
| 405 | `methodNotAllowed` | 非 POST |
| 500 | `insertFailed` | 写库失败 |

鉴权失败由 CPA 宿主直接返回，形如：

```json
{
  "error": "missing management key"
}
```

没有插件的 `error.code` 包装。

---

## 7. curl

```bash
curl -sS -X POST 'http://127.0.0.1:<cpa-port>/v0/management/cpa-xai-ip-switcher/nodes' \
  -H 'Authorization: Bearer <management-key>' \
  -H 'Content-Type: application/json' \
  -d '{
    "ips": [
      "socks5://user:pass@38.244.38.76:8888",
      "http://user:pass@1.2.3.4:8080"
    ]
  }'
```

---

## 8. 边界

- 本接口不能录入【健康保底】。
- 探测结果不在本次响应里；看插件 WebUI 批次 / 节点列表 / 探测日志。
- 代码已写入插件源码；上线需重新编译并替换插件 `.so`。
