"这是改造前的代码审计记录，包含我在阅读源码过程中发现的全部问题（含我自己判断失误并纠正的过程）。"
# go-zero-looklook 学习笔记（结业汇总）

> 这份文档是 8 讲陪伴式学习的汇总索引 + 速查手册 + 问题清单。
> **所有结论都经过实测验证**（查数据库、发 HTTP 请求、读 Redis、查 ES/Jaeger/Prometheus）。
> 凡是我推断而非验证的内容，都明确标注了「推断」。

**怎么用这份文档**

| 你想做的事 | 看哪一节 |
|---|---|
| 快速回忆项目长什么样 | [1. 项目全景](#1-项目全景) |
| 要连数据库 / 调接口 / 重启服务 | [2. 环境速查](#2-环境速查) |
| 复习某一讲的核心结论 | [3. 八讲索引](#3-八讲索引) |
| 看这个项目有哪些坑和缺陷 | [4. 发现清单](#4-发现清单) |
| 读一个新项目时该用什么手法 | [5. 可复用审计方法](#5-可复用审计方法) |
| 找练习题目 | [6. 作业清单](#6-作业清单) |

---

## 1. 项目全景

**go-zero-looklook** —— 民宿预订业务的微服务实战项目，go-zero 生态里最完整的开源案例之一。
价值不在业务复杂度，而在**工程链路的完整性**：写代码 → 生成代码 → 服务治理 → 可观测 → K8s 部署。

### 1.1 代码结构

```
app/                        5 个业务服务，每个都是同一种「三件套」结构
├── usercenter/  用户中心    ┐
├── travel/      民宿/旅行   │  cmd/api/  ← HTTP 聚合层，面向前端小程序
├── order/       订单        │  cmd/rpc/  ← gRPC 业务层，服务间互相调用
├── payment/     支付        │  cmd/mq/   ← Kafka 消费者（只有 order 有）
└── mqueue/      异步任务    │  model/    ← 数据访问层（goctl 生成）
                             ┘  cmd/job/       asynq worker（消费任务）
                                cmd/scheduler/ asynq 定时任务注册器
pkg/                        公共能力库
├── middleware/   JWT 鉴权中间件（★ 死代码）   ├── ctxdata/    uid 上下文透传
├── interceptor/  rpc server 拦截器（★ 核心）  ├── xerr/       错误码体系
├── result/       响应封装（HTTP/Job）        ├── kqueue/     Kafka 消息契约
├── globalkey/    全局常量（软删除、时间格式） ├── uniqueid/   单号生成
├── tool/         工具（金额、加密、随机串）   ├── wxminisub/  微信模板 ID
└── wxminisub/    微信订阅消息模板常量
deploy/                     部署与配置
├── nginx/conf.d/looklook-gateway.conf   网关路由（唯一可直接用的部署产物）
├── filebeat/conf/filebeat.yml           日志采集
├── go-stash/etc/config.yaml             日志管道
├── prometheus/server/prometheus.yml     监控抓取配置
├── goctl/1.7.3.zip                      ★ goctl 模板包（含 k8s 模板！）
├── script/gencode/gen.sh                代码生成命令备忘
└── sql/looklook_*.sql                   ★ 只有建表语句，没有 INSERT
doc/chinese/01~15.md        15 篇配套中文教程（部分已过时，见 4.5）
```

### 1.2 核心设计约定（理解全项目的钥匙）

> **api 层做聚合和鉴权，rpc 层做业务。** 简单、不被其他服务复用的逻辑可以直接写在 api 的 logic 里。

**但这条原则在实践中被两种相反的做法打破**：

| 服务 | api 层是否直连数据库 | 说明 |
|---|---|---|
| `usercenter/api` | ❌ **完全不碰 DB** | 纯转发：只做参数翻译 + 调 rpc |
| `travel/api` | ✅ **直连 DB**（注入 4 个 model） | 7 个 logic 里 6 个直接查库，只有 `homestayDetail` 走 rpc |
| `order/api` / `payment/api` | ❌ 不碰 DB | 纯转发 |

**为什么 travel 不一样？** 它的查询是「列表/分页/多条件筛选」，只给前端用、不被其他服务复用，走 rpc 只是无意义的转发。**这是权衡，不是错误** —— 代价是两个进程各持一套 DB/Redis 连接、数据访问逻辑分散。

### 1.3 一个请求的完整链路

```
小程序
  │  POST http://127.0.0.1:8888/usercenter/v1/user/register
  ▼
Nginx 网关 (8888)  ──按 URL 前缀转发──►  api 服务 (1004)
  ▼
routes.go          路由匹配（由 .api 文件生成）
  ▼
xxxHandler.go      解析 JSON → types.XxxReq，调 result.HttpResult 统一返回
  ▼
xxxLogic.go (api)  组装 rpc 请求 → 发起 gRPC 调用
  ▼ ══════════ 跨进程！网络！══════════
xxxServer.go (rpc) 收到 gRPC 请求，分发到 logic（由 .proto 生成）
  ▼
xxxLogic.go (rpc)  ★ 真正的业务逻辑
  ▼
model 层  →  MySQL + Redis（go-zero 自动缓存）
```

### 1.4 异步链路（三条，全部实测跑通）

```
① 延迟队列（asynq / Redis）
   下单 → Enqueue(ProcessIn(30min)) → asynq:{default}:scheduled
   → forwarder 发现到点 → pending → mqueue-job worker 消费
   → 跨服务调 order-rpc → 30 分钟未支付则关单

② 发布订阅（Kafka + go-queue/kq）
   支付成功 → kq.Pusher.Push → topic: payment-update-paystatus-topic
   → order-mq 消费（group: payment-update-paystatus-group）
   → 调 order-rpc → 订单状态 0(待支付) → 1(未使用)

③ 定时任务（asynq Scheduler / Redis）
   scheduler.Register("*/1 * * * *") → 每分钟产生结算任务 settle_record
```

**订单状态机**（`updateHomestayOrderTradeStateLogic.verifyOrderTradeState`）：

```
-1 已取消 ◄── 仅从 0 待支付
 0 待支付 ──► 1 未使用（支付成功）
 1 未使用 ──► 2 已使用 / 3 已退款 / 4 已过期
（不允许改成 0）
```

---

## 2. 环境速查

### 2.1 凭据（都硬编码在 `etc/*.yaml` 里，已提交进 git）

| 组件 | 地址 | 凭据 |
|---|---|---|
| MySQL | `127.0.0.1:33069` | `root` / `PXDN93VRKUm8TeE7` |
| Redis | `127.0.0.1:36379` | 密码 `G62m50oigInC30sf` |
| JWT Secret | — | `ae0536f9-6450-4606-8e13-5a19ed505da0` ⚠️ **api 与 rpc 必须一致** |
| Kafka（宿主机） | `127.0.0.1:9094` | 无认证 |
| 微信小程序 | — | `wx2add729fadddddd`（**假值**，支付链路因此跑不通） |

### 2.2 端口规划（约定优于配置）

| 服务 | api | rpc | metrics |
|---|---|---|---|
| order | 1001 | 2001 | 4001 / 4002（mq 是 4003） |
| payment | 1002 | 2002 | 4004 / 4005 |
| travel | 1003 | 2003 | 4006 / 4007 |
| usercenter | 1004 | 2004 | 4008 / 4009 |
| mqueue | — | — | job 4010 / scheduler 4011 |

**规律**：api=`1000+N`，rpc=`2000+N`，metrics=`4000+N`，宿主映射用 `3xxxx`。

### 2.3 Web 入口

| 界面 | 地址 |
|---|---|
| API 网关 | http://127.0.0.1:8888 |
| Kibana（日志） | http://127.0.0.1:5601 |
| Jaeger（链路追踪） | http://127.0.0.1:16686 |
| Prometheus（指标） | http://127.0.0.1:9090 |
| Grafana（面板） | http://127.0.0.1:3001 ⚠️ **零个预置面板** |
| asynqmon（队列） | http://127.0.0.1:8980 |
| Elasticsearch | http://127.0.0.1:9200 |

### 2.4 常用命令

```powershell
# ---- 启动中间件环境（只起 MySQL + Redis 最省资源）----
docker compose -f docker-compose-env.yml up -d mysql redis

# ---- 启动完整容器栈（含 nginx 网关）----
docker compose up -d

# ---- 本机原生跑 usercenter（需要 etc/*.local.yaml，见 2.5）----
cd app\usercenter\cmd\rpc ; go run usercenter.go -f etc/usercenter.local.yaml
cd app\usercenter\cmd\api ; go run usercenter.go -f etc/usercenter.local.yaml

# ---- 改端口后要按端口精准停掉本机进程 ----
Get-NetTCPConnection -LocalPort 1004,2004 -State Listen |
  ForEach-Object { Stop-Process -Id $_.OwningProcess -Force }

# ---- 连 MySQL ----
$env:MYSQL_PWD='PXDN93VRKUm8TeE7'
& 'C:\Program Files\MySQL\MySQL Server 9.2\bin\mysql.exe' -h 127.0.0.1 -P 33069 -u root --default-character-set=utf8mb4

# ---- 编译（如果 go-build 缓存被沙箱限制，把 GOCACHE 指到工作区内）----
$env:GOCACHE='D:\learning\go-zero-looklook\data\gocache' ; go build ./...
```

### 2.5 本机原生运行的关键改动

容器里的服务用容器主机名（`mysql:3306`、`redis:6379`）；**宿主机跑服务必须换成映射端口**：

| 原值（容器内） | 新值（宿主机） | 说明 |
|---|---|---|
| `mysql:3306` | `127.0.0.1:33069` | 3306 在宿主机映射成了 33069 |
| `redis:6379` | `127.0.0.1:36379` | 同上 |
| `http://jaeger:14268` | `http://127.0.0.1:14268` | Jaeger collector 端口有映射 |

**做法**：复制一份 `etc/usercenter.local.yaml` 改地址，用 `-f` 参数切换，**不要改原文件**（原文件容器栈还在用）。
`.gitignore` 已加 `**/*.local.yaml`，不会污染仓库。

### 2.6 已验证可用的接口

```powershell
$gw='http://127.0.0.1:8888'

# 注册（无需 JWT）
curl.exe -s -X POST $gw/usercenter/v1/user/register -H "Content-Type: application/json" `
  -d '{\"mobile\":\"13900001111\",\"password\":\"123456\"}'

# 登录（无需 JWT）
curl.exe -s -X POST $gw/usercenter/v1/user/login -H "Content-Type: application/json" `
  -d '{\"mobile\":\"13900001111\",\"password\":\"123456\"}'

# 查详情（需 JWT，Authorization: Bearer <token>）
curl.exe -s -X POST $gw/usercenter/v1/user/detail -H "Content-Type: application/json" `
  -H "Authorization: Bearer <TOKEN>" -d '{}'

# 民宿列表（公开）
curl.exe -s -X POST $gw/travel/v1/homestay/homestayList -H "Content-Type: application/json" `
  -d '{\"page\":1,\"pageSize\":5}'

# 下单（需 JWT）
curl.exe -s -X POST $gw/order/v1/homestayOrder/createHomestayOrder -H "Content-Type: application/json" `
  -H "Authorization: Bearer <TOKEN>" `
  -d '{\"homestayId\":11,\"isFood\":true,\"liveStartTime\":1790956800,\"liveEndTime\":1791129600,\"livePeopleNum\":2,\"remark\":\"test\"}'

# 订单详情（需 JWT；注意：非本人订单返回 HTTP 200 + data:null）
curl.exe -s -X POST $gw/order/v1/homestayOrder/userHomestayOrderDetail -H "Content-Type: application/json" `
  -H "Authorization: Bearer <TOKEN>" -d '{\"sn\":\"HSO...\"}'
```

### 2.7 数据库是空的（重要）

`deploy/sql/*.sql` **只有 `CREATE TABLE`，一条 `INSERT` 都没有**。
四个库（`looklook_usercenter` / `looklook_travel` / `looklook_order` / `looklook_payment`）都是空表。

**要跑通订单/支付/异步链路，必须先造数据。** 本项目学习过程中用的最小种子数据：

```sql
USE looklook_travel;
INSERT INTO homestay_business (id,title,user_id,info,boss_info,row_state,star,tags,cover,header_img,version)
VALUES (1,'山间小筑',3,'藏在山谷里的民宿','房东老王','','',1,4.8,'亲子','http://x/c.jpg','http://x/h.jpg',0);
INSERT INTO homestay (id,title,sub_title,banner,info,people_num,homestay_business_id,user_id,
                      row_state,row_type,food_info,food_price,homestay_price,market_homestay_price,version)
VALUES (11,'山谷观景房','推窗即见云海','http://x/b1.jpg,http://x/b2.jpg','可以看云海的小屋',2,
        1,3,1,0,'含双人早餐',3000,50000,68000,0);
INSERT INTO homestay_activity (id,row_type,data_id,row_status,version)
VALUES (1,'preferredHomestay',11,1,0),(2,'goodBusiness',3,1,0);
```

**注意**：`food_price` / `homestay_price` 的单位是**分**（3000 = 30 元）。

---

## 3. 八讲索引

### 第一讲 · 一张地图 + usercenter 全链路

- go-zero 是**带代码生成器的框架**，目录结构被钉死；`DO NOT EDIT` 的文件一个字都别改
- 固定四件套：`.api`（合同，人写）→ `routes/handler/types`（生成）→ `logic`（**唯一写业务的地方**）→ `svc`（依赖容器）
- `svc` 是全局依赖仓库（rpc 客户端只建一次），`logic` 每请求一个 —— **这个模式在项目里出现上百次**
- `rest.WithJwt(...)` 是**路由层面**的鉴权，token 不对根本进不到业务逻辑
- `AuthKey`/`AuthType` 设计让一张 `user_auth` 表支持多种登录方式（手机号 / 微信）

📁 `app/usercenter/cmd/api/{usercenter.go, internal/{config,svc,handler,logic}}`

### 第二讲 · JWT 闭环与错误处理三层

- **JWT 闭环**：rpc 签发（HS256 + secret + `jwtUserId`）→ api 用**同一个 secret** 验签（`rest.WithJwt`）→ 业务用 `ctxdata.GetUidFromCtx(ctx)` 取 uid
- ⚠️ **`json.Number` 是隐形地雷**：JWT payload 是 JSON，go-zero 用 `Decoder.UseNumber()`。写成 `.(float64)` 断言会**永远失败且不报错**，uid 恒为 0
- **错误三层**：`xerr.CodeError` 定义 → `errors.Wrapf` 传播（保留堆栈+上下文）→ `result.HttpResult` 返回
- ⭐ **错误白名单是安全底线**（`xerr.IsCodeErr`）：只有登记在 `message` 表里的业务错误才透给前端，DB/底层错误一律替换成"服务器开小差啦"。**实测**：rpc 挂掉时真实错误含 `127.0.0.1:2004`，前端只看到兜底文案

📁 `pkg/xerr/*.go`、`pkg/ctxdata/ctxData.go`、`pkg/result/httpResult.go`、`app/usercenter/cmd/api/internal/handler/routes.go`

### 第三讲 · 开发闭环与故障实验

- **两套栈**：容器栈（走网关 `8888`，配置用容器主机名）/ 本机栈（直连 `1004`，配置用 `127.0.0.1:3xxxx`）
- **坑**：`data/server/*`（约 70MB/个）**不是日志，是 modd 编译出的 Linux 二进制**
- **坑**：`looklook` 容器**没有映射** 1004/2004，宿主机探测"closed"不代表服务没跑；唯一正确的验证路径是走网关
- 本机栈用 `-f etc/xxx.local.yaml` 切换配置，**不动原文件**
- **故障实验**：杀掉 rpc → api 返回 `{"code":100001,"msg":"服务器开小差啦"}`，服务端日志里保留完整原始错误（含内部 IP:端口）—— **实证了错误白名单的价值**

### 第四讲 · model 层双 key 缓存与三道防护

- **缓存 key 格式**：`cache:{库名驼峰}:{表名}:{索引列}:{值}`
  → 如 `cache:looklookUsercenter:user:mobile:13777778888`
  → ⚠️ **改数据库名 = 缓存 key 前缀全变 = 缓存瞬间全失效**
- ⭐ **双 key 设计**（实测值）：
  - 索引 key（`user:mobile:xxx`）→ 值只有**主键**（`"3"`，1 字节）
  - 主键 key（`user:id:3`）→ 值是**整行 JSON**（277 字节）
  - 好处：**数据只存一份**，改数据时只需删这两个 key，永不漏删
- ⭐ **5 秒安全间隔**：`cacheSafeGapBetweenIndexAndPrimary = time.Second * 5`
  - **实测两个 key 的 TTL 差值恰好 5 秒**，与源码常量精确吻合
  - 目的：让"指针"（索引 key）比"数据"（主键 key）**先过期**，避免陈旧指针指向空缓存
- **三道防护**（全部有源码 + 实测证据）：

  | 问题 | 机制 | 位置 |
  |---|---|---|
  | 击穿 | SingleFlight：同 key 并发只查 1 次 DB | `cachenode.go` `barrier.DoEx` |
  | 穿透 | 空值占位符 `"*"`，TTL 1 分钟，SETNX 写入 | `setCacheWithNotFound` |
  | 雪崩 | TTL 随机抖动 ±5% | `mathx/unstable.go` |

  + **快速失败**：Redis 故障时**绝不降级去查 DB**（源码注释："we don't allow the disaster pass to the dbs"）
- **写操作是 Cache-Aside**：`ExecCtx` 先写 DB，成功后再**删**缓存（删比更新幂等）

📁 `app/usercenter/model/userModel_gen.go`、`go-zero@v1.7.3/core/stores/{sqlc,cache}/*.go`

### 第五讲 · pkg/ 公共组件存活审计

- ⭐ **读陌生项目的第一个动作：grep 引用计数，分清死活**
  - 审计结果：27 个关键符号里 **10 个是死代码**（37%）
- ⭐ **拦截器是跨进程错误传播的关键**（`pkg/interceptor/rpcserver/loggerInterceptor.go`，39 行）：
  ```go
  err = status.Error(codes.Code(e.GetErrCode()), e.GetErrMsg())
  ```
  **把业务错误码（100001）塞进 gRPC 状态码字段** —— 实测日志里出现 `code = Code(100001)`。
  - 这是 **hack**（gRPC 状态码规范取值 0~16），正统做法是 `status.WithDetails`
  - 但两端配套（api 侧 `status.FromError` + `IsCodeErr` 白名单），链路是通的
- `errors.Wrapf` 的回报：日志里有**精确到行**的完整堆栈 + 业务上下文（`mobile:xxx`、`userId:3`）
- **go-zero 在暗中做的事**（从 stack trace 里发现的）：`zrpc` 自动挂了 `UnaryTimeoutInterceptor`
- `globalkey.DelStateNo` 被引用 **86 次** —— 魔法值单点化的教科书案例
- `CacheUserTokenKey` 是死常量，但它是**"被放弃的 token 撤销设计"的化石**
- `tool.Fen2Yuan` 20 次调用**全在 api 层** → 架构边界清晰：**rpc 传"分"，api 转"元"**
- `tool.Yuan2Fen` 零调用 → **反推出"客户端无法传金额进来"（安全正面）**

### 第六讲 · 从"读代码"到"审代码"（挖出真 Bug）

- **越权审计结论**：订单接口**无 IDOR 漏洞**
  - 下单：`userId := ctxdata.GetUidFromCtx(ctx)`，且 `.api` 定义里**没有 userId 字段**，客户端无法伪造
  - 查详情：`if resp.HomestayOrder.UserId == userId` 做了归属权校验
  - **实测**：用户 B 查用户 A 的订单 → `HTTP 200` + `{"code":200,"msg":"OK","data":null}`
  - ⚠️ **安全上正确**（不泄露订单是否存在），**但 API 语义错误**（200 表示成功，客户端必须特判 null）
- `resp.HomestayOrder != nil` 是**死逻辑** —— rpc 永远返回 `&resp`（零值结构体的地址），永远非 nil
- 🔴 **发现真 Bug**：`order.proto` 有 `int64 needFood = 27;`，但 `order.pb.go` 的字段号只到 26
  - **proto 改了但没重新生成 Go 代码** → `copier.Copy` 静默跳过该字段
  - **实测证据**：MySQL 里 `need_food=1`，API 返回 `needFood:0`
  - 系统性审计：4 个 proto 里**只有 order 不同步**（孤立的疏漏，不是流程问题）
- **延迟队列实证**：`defer:homestay_order:close` 载荷含订单号，`timeout=1800`(30 分钟)

### 第七讲 · 异步链路全景

- ⭐ **延迟队列闭环**（用 `ZADD` 改分数把任务提前，免等 30 分钟）
  - 实测：`trade_state` `0 → -1`，`update_time` 随之更新
- ⭐ **Kafka 解耦实测**：自己写发布程序往 `payment-update-paystatus-topic` 发消息
  - 实测：`trade_state` `0 → 1`（走了"支付成功"分支）
  - **证明了解耦的价值**：只要消息符合契约，消费方就正确响应，不关心消息来自谁
- **异步链的续集**：状态变成"未使用"→ 自动入队 `msg:pay_success:notify_user` → 执行失败（用户没有微信 openid）→ **进 retry 队列，错误信息存进任务载荷**
- ⚠️ **三个双写不一致隐患**（本讲最重要的发现）：

  | # | 操作 | 失败处理 | 后果 |
  |---|---|---|---|
  | ① | 创建订单：`Insert` + `asynq.Enqueue` | 只 `Errorf` | 订单永不会超时关闭 |
  | ② | 支付更新：`Update` + `Kafka.Push` | 只 `Errorf` | **用户付了钱，订单还显示待支付** |
  | ③ | 订单变更：`Update` + `asynq.Enqueue` | 只 `Errorf` | 用户收不到通知 |

- ⚠️ **`kq.Pusher.Push` 是缓冲的**（源码验证）：走 `ChunkExecutor`，`Push` 返回 ≠ 消息已发出；
  **Kafka 挂了它依然返回 `nil`**，真正的错误只在 executor 内部被 `logx.Error` 掉。
  → **`if err != nil` 这种错误处理几乎等于没写**
- ✅ **确定结论：`dtm` 在本项目中完全不存在**（go.mod 无依赖、compose 无容器、全仓库零匹配）。README 那句话是纯愿景

### 第八讲 · 可观测与部署

- **日志链路**：`应用 stdout → Docker 落盘 → filebeat 读文件 → Kafka(looklook-log) → go-stash → ES → Kibana`
  - ✅ **实测**：在 ES 里搜到了自己上一讲触发的日志，**恰好 2 条 = 2 次重试**（间隔 21 秒）
  - ⚠️ **三个真问题**：
    ① go-zero 的 JSON 日志**没被解析**，整条塞进 `data.log` 字符串 → go-stash 的 drop 规则引用 `level`/`k8s_container_name`，**两个字段都不存在 → 规则失效**
    ② `orderSn:HSO...` 被 standard 分词器当成**一个 token**（UAX#29 里冒号是 MidLetter）→ **单搜订单号 0 命中**
       修法：`data.log.keyword:*SN*` 或 `data.log:"orderSn:SN"`；且 `ignore_above:256` 让长日志行在 keyword 里也搜不到
    ③ filebeat 读的是 **Docker 日志文件** → **本机 `go run` 的服务日志进不了 ES**
- **链路追踪**：Jaeger 里 **8 个服务**全注册（含 `mqueue-job`）；⚠️ 跨 Kafka/asynq 的**异步边界会断链**（trace ID 没塞进消息体）
- **监控**：Prometheus **12 个目标全 `up`**；⚠️ **Grafana 零个预置面板**
- **部署**：
  - 开发：`docker-compose-env.yml`（中间件）+ `docker-compose.yml`（应用 + 网关）
  - 生产：`gitlab + jenkins + harbor + k8s`
  - ⭐ **K8s 编排不在仓库里 —— 是 goctl 模板生成的**，模板在 `deploy/goctl/1.7.3.zip` 的 `1.7.3/kube/deployment.tpl`
    - 生成：Deployment（**readiness/liveness 探针**、资源限制、时区挂载、`serviceAccountName`）+ Service（NodePort）+ **两个 HPA**
    - ⚠️ 模板用了 `autoscaling/v2beta2`，**在 k8s 1.26+ 已被移除 → `kubectl apply` 会失败**
  - ⭐ **服务发现机制**：k8s 的 Service/Endpoints **本身就是注册表**；go-zero 用 `find-endpoints` 这个 ServiceAccount 的 `get/list/watch endpoints` 权限**直接 watch** → **所以真的不需要 etcd/nacos/consul**
    - （`etcd` 在 go.mod 里，但都是 `// indirect` —— 只是 go-zero zrpc 的传递依赖）
  - ⭐ **为什么不用配置中心**：作者的选择 —— 配置打进镜像，换来**回滚原子性**（`kubectl rollout undo` 一次回滚代码+配置）。代价是改配置要走完整发布流程

---

## 4. 发现清单

> 按严重度排序。这些是学习这个项目的**最大收获** —— 一个能跑的项目里藏着多少问题。

### 4.1 🔴 严重（会导致错误行为）

| # | 问题 | 证据 | 影响 |
|---|---|---|---|
| 1 | **`order.proto` 与 `order.pb.go` 不同步**，`needFood`(字段号 27) 缺失 | DB `need_food=1` / API 返回 `needFood:0`；proto 最大字段号 27 vs pb.go 26 | 用户选了"需要餐食"（价格也算了餐费），详情页却显示不需要。**每次查询都触发** |
| 2 | **三处"双写不一致"只记日志不返回错误** | 代码位置见第七讲 | 支付成功但订单状态没变等静默不一致 |
| 3 | **`kq.Pusher.Push` 是缓冲的**，Kafka 故障时仍返回 `nil` | `go-queue@v1.1.8/kq/pusher.go:64` | 支付成功→Kafka 这条链路"看起来成功"，实际可能丢消息 |
| 4 | **`deployment.tpl` 用 `autoscaling/v2beta2`** | 模板第 97 行 | k8s 1.26+ 上 `kubectl apply` 直接失败 |

### 4.2 🟠 中等（功能/运维缺失）

| # | 问题 | 证据 |
|---|---|---|
| 5 | **Grafana 零个面板** —— 指标采了但没人看 | `/api/search?type=dash-db` 返回 0 |
| 6 | **go-stash 的 drop 规则失效** —— 引用不存在的字段 `level`/`k8s_container_name` | ES 映射只有 `@timestamp, agent, data, ecs, host, input, log` |
| 7 | **ES 里日志是未解析的字符串** —— 无法按 `level`/`caller`/`trace` 聚合分析 | 同上 |
| 8 | **`key:value` 被切成一个 token** → 订单号/请求 ID 搜不到 | `_analyze` 输出 `ordersn:hso...` 为单 token |
| 9 | **`ignore_above: 256`** → 超过 256 字符的日志行在 keyword 字段里也搜不到 | ES 字段映射 |
| 10 | **`deploy/sql/*.sql` 只有建表语句**，没有数据 | grep `INSERT` 零匹配 |
| 11 | **`data/server/*` 是 Linux 二进制**（约 70MB/个），容易被误认为日志 | modd.conf 的 `go build -o data/server/...` |
| 12 | **无告警规则** —— Prometheus 没有 alerting rules | 只有 `prometheus.yml` 的抓取配置 |

### 4.3 🟡 轻微 / 代码质量问题

| # | 问题 | 说明 |
|---|---|---|
| 13 | 越权访问返回 `HTTP 200 + data:null` | 安全上正确（不泄露存在性），但语义错误 |
| 14 | **401 响应体为空**，不走统一 `{code,msg}` 信封 | go-zero 内置 JWT 中间件直接返回，不经过 `pkg/result` |
| 15 | 登录错误泄露"手机号是否已注册" | `ErrUserNoExistsError` vs `ErrUsernamePwdError` 是两个不同消息 |
| 16 | **密码用无盐 MD5** | `pkg/tool/encryption.go` |
| 17 | **明文密码会进日志**（登录失败时） | go-zero 客户端拦截器把整个请求结构体打进日志 |
| 18 | **JWT 有效期 365 天且无法撤销** | `AccessExpire: 31536000`；`CacheUserTokenKey` 化石证明曾想做成可撤销 |
| 19 | **Redis 里缓存了含密码哈希的整行用户数据** | `select <所有字段>` 生成 `userRows` |
| 20 | 错误消息中英混杂 | `"user has been registered"` vs 其他中文 |
| 21 | **`tool.Krand` 用 `math/rand` + 已弃用的 `rand.Seed`** | 订单号可预测（但归属权校验在，风险可控） |
| 22 | 金额计算里混用 `float64` | `createHomestayOrderLogic` 用 proto 的 float64 价格直接相乘 |
| 23 | `prometheus.yml` 的 `global.scrape_interval` 是**空值** | 第 2 行 `scrape_interval:` 后面没写值 |
| 24 | 业务 model 从**脚本目录**导入错误常量 | `looklook/deploy/script/mysql/genModel` —— 删掉 `deploy/script` 会编译失败 |
| 25 | `pkg/result` 里 `HttpResult` 与 `JobResult` 有 15 行重复的错误提取逻辑 | 违反 DRY |
| 26 | **`RefreshAfter` 返回了却没有刷新接口** | 响应里有 `refreshAfter`（"过半有效期该续期"），但**全项目没有任何 `/refresh` 端点**（grep 验证）；token 一旦过期只能重新登录 |

### 4.4 死代码 / 不存在的功能

**`pkg/` 里零引用的符号**（共 10 个）：

```
uniqueid.GenId                              globalkey.CacheUserTokenKey
globalkey.DateTimeFormatTplStandardDateTime globalkey.DateTimeFormatTplStandardTime
result.JobResult                            result.AuthHttpResult
tool.Yuan2Fen                               tool.InPlaceholders
xerr.NewErrCodeMsg                          middleware.NewCommonJwtAuthMiddleware
```
另外 `app/usercenter/cmd/api/internal/svc/serviceContext.go` 的 `SetUidToCtxMiddleware` 字段声明了但从未赋值（恒为 nil）。

**完全不存在的东西**：

| 声称存在 | 实际情况 |
|---|---|
| `dtm` 分布式事务 | **零代码、零依赖、零容器**（README 说是"准备使用"，实际连依赖都没引入） |
| 各服务的 `Dockerfile` | 仓库里**一个都没有**（doc/15 说"创建项目时就放在目录下"） |
| `identity-api` / `identity-rpc` 服务 | doc/03 里描述的流程提到了，**当前代码库没有这个服务** |
| K8s 编排清单 | 仓库里没有，但**不是缺失** —— 由 `deploy/goctl/*.zip` 里的模板生成 |

### 4.5 文档漂移汇总

| 文档说 | 实际 |
|---|---|
| `cp -r data/goctl ~/.goctl`（doc/03:74） | 模板在 `deploy/goctl/` |
| 有 `identity` 鉴权服务（doc/03:136） | 不存在 |
| Dockerfile 在项目目录下（doc/15:246） | 不存在 |
| 有 K8s 部署教程 → 可直接 `kubectl apply` | 编排文件是部署时由 goctl 生成的，且模板在现代 k8s 上会失败 |
| 用 dtm 做分布式事务（README:121） | 完全不存在 |

---

## 5. 可复用审计方法

> 这一节是 8 讲里**最可迁移**的部分 —— 换成任何项目都能用。

### 5.1 第一步永远是：分清死活

```powershell
# 统计某个符号被引用了多少次（排除定义所在的包）
$syms = @('uniqueid.GenId','result.JobResult','tool.Yuan2Fen')   # 换成你要查的
$files = Get-ChildItem app, pkg -Recurse -Filter *.go | Where-Object { $_.Name -notlike '*_test.go' }
foreach ($s in $syms) {
  $pat = [regex]::Escape($s); $pkgName = ($s -split '\.')[0]; $hits = 0
  foreach ($f in $files) {
    if ($f.FullName -like "*\pkg\$pkgName\*") { continue }
    $hits += ([regex]::Matches((Get-Content $f.FullName -Raw), $pat)).Count
  }
  "{0,-40} {1}" -f $s, $(if ($hits -eq 0) { '★ 未被使用' } else { $hits })
}
```

**为什么必须先做这一步**：我第四讲就是没做，白读了一个从没被调用的函数，还得出错误结论。

### 5.2 proto 与生成代码是否同步

```powershell
# 比对每个 proto 和它生成的 pb.go 的最大字段号
foreach ($p in (Get-ChildItem app -Recurse -Filter *.proto)) {
  $go = $p.FullName -replace '\.proto$', '.pb.go'
  if (-not (Test-Path $go)) { continue }
  $pn = [regex]::Matches((Get-Content $p.FullName -Raw), '=\s*(\d+)\s*;') | ForEach-Object { [int]$_.Groups[1].Value }
  $gn = [regex]::Matches((Get-Content $go -Raw), 'protobuf:"[^"]*?,(\d+),') | ForEach-Object { [int]$_.Groups[1].Value }
  $pmax = ($pn | Measure-Object -Maximum).Maximum; $gmax = ($gn | Measure-Object -Maximum).Maximum
  "{0,-30} proto={1,3}  pb.go={2,3}  {3}" -f $p.Name, $pmax, $gmax, $(if ($pmax -gt $gmax) { '🔴 不同步' } else { '🟢' })
}
```

**这个方法在本项目里抓出了一个真 Bug**（`needFood` 字段）。

### 5.3 不装客户端也能读 Redis（原始 TCP 说 RESP 协议）

```powershell
function RedisCmd([string[]]$cmds) {
  $c = New-Object System.Net.Sockets.TcpClient('127.0.0.1', 36379); $s = $c.GetStream()
  $w = New-Object System.IO.StreamWriter($s, [System.Text.Encoding]::ASCII); $w.NewLine = "`r`n"; $w.AutoFlush = $true
  $w.WriteLine("AUTH G62m50oigInC30sf"); Start-Sleep -Milliseconds 200
  foreach ($x in $cmds) { $w.WriteLine($x); Start-Sleep -Milliseconds 250 }
  Start-Sleep -Milliseconds 600
  $sb = New-Object System.Text.StringBuilder
  while ($s.DataAvailable) { $b = New-Object byte[] 65536; $n = $s.Read($b,0,65536); if ($n -le 0) { break }; [void]$sb.Append([System.Text.Encoding]::UTF8.GetString($b,0,$n)) }
  $c.Close(); return $sb.ToString()
}
RedisCmd @('KEYS cache:*','TTL cache:looklookUsercenter:user:id:3')
```

**用途**：查看缓存 key 的真实形态、验证 TTL 抖动、看 asynq 队列。
**注意**：函数名别用 `R`（会撞 PowerShell 内置别名 `Invoke-History`）。

### 5.4 把时间相关的任务"提前"来验证

```powershell
# 把 asynq 延迟任务的计划时间改成"现在"，forwarder 会在 1 秒内推进它
RedisCmd @("ZADD asynq:{default}:scheduled $([DateTimeOffset]::UtcNow.ToUnixTimeSeconds()) <任务ID>")
```

**用途**：测试延迟队列、定时任务、过期策略 —— 不用真的等。
**这是测试手段，不要用在业务代码里。**

### 5.5 "DB 原始值 vs API 返回值"对照法 ⭐

**这是抓出 `needFood` Bug 的唯一方法**，也是最容易被忽略的一步。

```powershell
$env:MYSQL_PWD='PXDN93VRKUm8TeE7'
$m='C:\Program Files\MySQL\MySQL Server 9.2\bin\mysql.exe'
# 1) 看数据库里存了什么
& $m -h 127.0.0.1 -P 33069 -u root --default-character-set=utf8mb4 -t -e "SELECT sn,user_id,need_food,order_total_price FROM looklook_order.homestay_order;"
# 2) 调接口看返回什么
curl.exe -s -X POST http://127.0.0.1:8888/order/v1/homestayOrder/userHomestayOrderDetail `
  -H "Content-Type: application/json" -H "Authorization: Bearer <TOKEN>" -d '{\"sn\":\"HSO...\"}'
# 3) ★ 把两者摆在一起比对
```

> **"HTTP 200 + 有数据"不等于接口正确。** 只要字段在传递过程中被静默丢弃（比如 `copier.Copy` 遇到目标结构体没有的字段），你永远发现不了。

### 5.6 ES 搜索诊断三板斧

```powershell
# ① 索引和文档数
GET http://127.0.0.1:9200/_cat/indices?v
# ② 字段映射 —— 数一下到底有几个字段（只有一个字符串字段 = 日志平台退化成文本仓库）
GET http://127.0.0.1:9200/looklook-2026-10-02/_mapping
# ③ 分词结果 —— "搜不到"时第一个该看的
POST http://127.0.0.1:9200/looklook-2026-10-02/_analyze
  {"field":"data.log","text":"orderSn:HSO2026100217162206278460"}
```

### 5.7 读代码 vs 审代码（方法论总结）

```
① 找入口和契约       读 .api 文件 → 接口收什么、返回什么
                     关键：请求体里有没有「不该由客户端决定」的字段（userId/price/status）
② 追信任边界         uid 从哪来？必须是 ctxdata.GetUidFromCtx(ctx)，绝不能来自请求参数
③ 读一层就问「下一层是谁」  api → rpc → model → DB，每层都看
                     关键：看返回值的「空值约定」（nil？零值结构体？）—— 本项目这里就没统一
④ 别只信代码，看真实数据 ★ 见 5.5 —— 这一步抓出了 needFood Bug
⑤ 看生成代码是否与源文件同步 ★ 见 5.2
⑥ 看外部依赖的副作用  同一份缓存/队列被几个进程共用？（travel/api 和 travel/rpc 共用同一套 Redis key）
⑦ 看「缺失的设计」    某函数零调用 → 反推「某类风险不存在」（Yuan2Fen 零调用 = 客户端传不了金额）
```

---

## 6. 作业清单

> 按难度排序，前两个是"动手改代码"，建议至少做掉。

### 6.1 修掉 `needFood` Bug（★ 推荐第一个做）

1. 备份 `app/order/cmd/rpc/pb/order.pb.go`
2. 按 `deploy/script/gencode/gen.sh` 里的命令重新生成（`goctl rpc protoc order.proto --go_out=../ --go-grpc_out=../ --zrpc_out=../ --style=goZero`）
3. 对比 diff，确认新增了 `NeedFood` 字段
4. 重启 order-rpc，再查一次订单详情，确认 `needFood` 变成 `1`
5. **注意**：重新生成后要检查其它字段有没有被改变（尤其是你之前修过的 `omitempty`）

### 6.2 修掉 `pkg/result` 的重复代码

把 `HttpResult` 和 `JobResult` 里重复的「错误 → (errCode, errMsg)」提取逻辑抽成一个内部函数，改完跑 `go build ./...` 确认编译通过。

### 6.3 修掉 `deployment.tpl` 的 HPA 问题

删掉 `autoscaling/v2beta2` 那个 HPA 块，把 memory metric 合并进 `autoscaling/v2` 的 HPA。用 `goctl kube deploy` 生成一次验证。

### 6.4 给 ES 加 Ingest Pipeline 解析日志

用 `json` processor 解析 `data.log`，让 `level`/`caller`/`span`/`trace` 变成真字段。验证：① 搜订单号能直接命中；② 能按 `level:"error"` 聚合。

### 6.5 修复日志过滤规则

基于 6.4 的成果，把 go-stash 的 drop 条件从 `k8s_container_name` 改成实际存在的字段（`container.name` 之类），验证 rpc 的 info 日志真的被丢弃。

### 6.6 思考题：修掉双写不一致

选隐患②（支付成功但 Kafka 没发）。具体回答：
- 改哪几个文件
- 要不要加 outbox 表
- relay 用什么触发（定时任务 or 常驻服务）
- **消息重复投递时订单侧会怎样**（提示：想想 `verifyOrderTradeState` 的状态机）

### 6.7 思考题：加"强制用户下线"

线索是 `globalkey.CacheUserTokenKey = "user_token:%d"` 这个死常量。设计一下：
- 签发 token 时写什么进 Redis
- 校验时怎么查（注意 `rest.WithJwt` 是框架内置的，要换成自定义中间件）
- 改动涉及哪几个文件（`pkg/middleware/commonJwtAuthMiddleware.go` 那个死文件正好可以用上）

### 6.8 观察任务：asynq 任务的完整生命周期

打开 http://127.0.0.1:8980 ，找到 `msg:pay_success:notify_user`。观察它的重试次数和下次重试时间（指数退避）。持续失败最终会进 `archived`（归档）队列 —— **观察完这个过程，你就理解了异步任务"彻底放弃"的完整生命周期**。

### 6.9 观察任务：跨异步边界断链

去 Jaeger UI 找 `service=mqueue-job` 的 trace，观察它是不是一条**孤立的小 trace**，和"支付成功"那条完全无关。思考：要关联起来需要在哪一步做什么。

---

## 7. 元教训（8 讲里我犯的错和纠正过程）

> 这部分比任何技术结论都重要。**我在这个项目上一共猜错了 3 次**，每次都是实验把我纠正过来的。

| 回合 | 我的猜测 | 真相 | 纠正方式 |
|---|---|---|---|
| 第二讲 | "密码错和用户不存在应该是同一个错误码，防手机号枚举" | ❌ 是两个不同错误，信息确实泄露 | 发真实请求，看到两条不同的 msg |
| 第四讲 | "索引查询不防缓存穿透" | ❌ 也防，只是占位符 TTL 只有 1 分钟，我查晚了 | 重新多点采样 + 读框架源码追到 `doTake` |
| 第五讲 | "`"Y-m-d H:i:s"` 是 PHP 语法，Go 用不了，这是个 Bug" | ❌ 项目用了 `carbon` 库，它就是 PHP 风格格式符 | 多问一句"它被谁消费" |

**三次翻车的共同点：看到一个局部，脑补出一个完整故事。**

**更隐蔽的第四次**：我读了 `QueryRowIndexCtx` 的错误分支就判定"不写占位符"，**但那一层只是壳** —— 它调用的 `TakeWithExpireCtx` 内部同样走 `doTake`，占位符正是 `doTake` 写的。**"我读过这段代码"不等于"我理解了这段代码"。**

### 三条可复用的纪律

1. **验证永远比阅读可靠。** 能实验证伪的判断，就不要只靠推理。
2. **结论前多问一句**：它被谁消费？谁调用它？在什么条件下生效？数据有时效性吗？
3. **读框架代码要追到真正的执行路径。** 看到包装函数就问一句"它内部调了谁"。

### 另外三条关于"读项目"的纪律

4. **"存在"不等于"生效"。** 死代码、死配置、死依赖（`etcd` 是 indirect）、不存在的功能（`dtm`）、过期的模板（`v2beta2`）—— **先分清死活，再花时间理解。**
5. **文档会落后于代码。** 本项目至少 5 处文档漂移（见 4.5）。**对任何"文档承诺"，都要用 grep 验证。**
6. **架构原则是权衡，不是教条。** `travel/api` 直连 DB、配置打进镜像、用 nginx 而不是 APISIX —— **这些不是对错，是取舍**。真正的能力是知道自己在破坏什么、为什么、付什么代价。

---

## 8. 本次学习对仓库的改动足迹

**改动最小化原则**：整个过程只修改了 **1 个**被 git 追踪的文件。

```
 M .gitignore                                    ← 唯一由我修改的追踪文件
 M app/usercenter/cmd/rpc/usercenter.go          ← 学习前就存在（1 个空行，gofmt 痕迹）
 M go.mod                                        ← 学习前就存在（仅 CRLF 行尾差异，无内容改动）
```

`.gitignore` 的新增内容（3 行）：

```gitignore
# local run configs (host-native debugging, see etc/*.local.yaml)
**/*.local.yaml
```

**新增的文件全部在 gitignore 覆盖范围内**（`data/*` 和 `**/*.local.yaml`）：

| 路径 | 用途 |
|---|---|
| `app/usercenter/cmd/{api,rpc}/etc/usercenter.local.yaml` | 本机原生运行的配置 |
| `data/seed.sql` | 最小种子数据（民宿/店铺/活动） |
| `data/kafkapub/main.go` + `.exe` | Kafka 发布程序（验证消费链路用） |
| `data/goctl-tpl/1.7.3/kube/deployment.tpl` | 从 `deploy/goctl/1.7.3.zip` 提取的 K8s 模板 |
| `data/gocache/` | Go 构建缓存（沙箱只允许写工作区内） |
| `data/lastsn.txt` | 测试用订单号 |

**清理方式**：删掉 `data/` 下的这些即可；`git checkout .gitignore` 可还原唯一改动。

**其他环境副作用**（不在仓库里）：

- MySQL 里新增了 3 个用户、1 个民宿、1 个店铺、2 个订单（学习过程产生）
- Redis 里有 asynq 的任务和 go-zero 的 model 缓存
- ES 里有我触发的日志

---

## 9. 学习资源

| 资源 | 地址 |
|---|---|
| go-zero 官网/文档 | https://go-zero.dev |
| go-zero 源码 | https://github.com/zeromicro/go-zero |
| go-queue（Kafka 封装） | https://github.com/zeromicro/go-queue |
| go-stash（日志管道） | https://github.com/kevwan/go-stash |
| asynq（延迟/定时队列） | https://github.com/hibiken/asynq |
| carbon（PHP 风格日期库） | https://github.com/golang-module/carbon |
| dtm（分布式事务，**本项目未使用**） | https://github.com/dtm-labs/dtm |
| 作者的 dtm 专项教程 | https://github.com/Mikaelemmmm/gozerodtm |
| 线上配置仓库（doc/15 提到） | https://github.com/Mikaelemmmm/go-zero-looklook-pro-conf |
| 项目自带中文教程 | `doc/chinese/01~15.md`（注意部分已过时） |

---

## 10. 一句话总结这个项目

> **它是一个优秀的"教学载体"，但不是一个可以直接抄的生产范本。**
>
> 它的价值在于**把微服务的完整工程链路摆在你面前** —— 代码生成、api/rpc 分层、服务治理、缓存设计、异步解耦、可观测、K8s 部署，每一环都有真实可运行的代码。
>
> 它的短板也很真实：**密码用无盐 MD5、JWT 一年不可撤销、缓存里存密码哈希、三处双写不一致、proto 改了忘记生成、K8s 模板用已移除的 API 版本、Grafana 零面板、文档落后于代码。**
>
> **能同时看到这两面，才是真正"学会了"这个项目。**
