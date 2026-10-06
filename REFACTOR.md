# 改造记录（REFACTOR）

本仓库 fork 自 [Mikaelemmmm/go-zero-looklook](https://github.com/Mikaelemmmm/go-zero-looklook)（MIT License）。
我在研读其源码的基础上，对其做缺陷修复、分布式一致性加固、安全加固与可观测性改造。

---

## 改造进度

| # | 改造项 | 状态 | 对应 commit |
|---|---|---|---|
| 1 | 修复 gRPC 代码生成漂移导致字段静默丢失 | ✅ 已完成 | `77a4a1e` |
| 2 | 引入事务性发件箱（Outbox）解决支付链路双写不一致 | 📋 计划中 | — |
| 3 | 安全加固：bcrypt 密码 / 日志脱敏 / JWT 缩短有效期 + jti 黑名单 | 📋 计划中 | — |
| 4 | 可观测性：修复失效的日志过滤规则、为 ES 配置 Ingest Pipeline | 📋 计划中 | — |

> **每完成一项，我会补上完整的「问题现象 / 定位过程 / 根因 / 修复 / 验证证据」，并附对应 commit。**
> 未完成的项只保留问题描述，不写未经验证的结论。

---

## 1. 修复 needFood 字段静默丢失

> 状态：✅ **已完成** —— 根因已定位并修复，通过编译 / 最小复现 / 端到端三层验证

### 问题现象

订单详情接口返回的 `needFood` 永远为 `0`，但数据库中该订单的 `need_food` 字段实际为 `1`。

### 定位过程

**第 1 步：对比「数据库原始值」与「接口返回值」**

数据库（事实基准）：

```
mysql> SELECT sn, need_food, order_total_price FROM looklook_order.homestay_order;
+---------------------------+-----------+-------------------+
| sn                        | need_food | order_total_price |
+---------------------------+-----------+-------------------+
| HSO2026100217125693042474 |         1 |            112000 |
+---------------------------+-----------+-------------------+
```

接口返回（`needFood` 是 0，但 `orderTotalPrice` 换算成元后是正确的 1120 —— 说明不是接口整体坏了，而是**单个字段**丢了）：

```json
{"orderTotalPrice":1120, "needFood":0, "...":"..."}
```

**第 2 步：逐层向上追溯数据流**

```
model.HomestayOrder                （有 NeedFood）
   │  copier.Copy(&resp, homestayOrder)            ← rpc 层
   ▼
pb.HomestayOrder                   （结构体缺该字段 → 静默丢弃）★
   │  copier.Copy(&typesOrderDetail, resp)         ← api 层
   ▼
types.UserHomestayOrderDetailResp  （有 NeedFood，但只拿到零值）
```

**第 3 步：比对 `.proto` 与生成的 `.pb.go`**

| 文件 | 最大字段号 | 是否有 needFood |
|---|---|---|
| `order.proto`（源文件） | **27**（`int64 needFood = 27;`） | ✅ |
| `order.pb.go`（生成物） | **26** | ❌ |

### 根因

**直接原因**：`.proto` 被修改后未重新生成 Go 代码（代码生成漂移）。

由于 `copier.Copy` 是反射实现、按字段名匹配，**目标结构体没有该字段时会被静默跳过、不报错**。
因此呈现出「编译通过、运行不报错、日志干净」的假象，但字段在 rpc → api 的传递途中已经丢失。

**根本原因（排查中发现的更深层问题）**：

`order` 服务与其他 3 个服务使用了**不同的代码生成工具链**：

| | `order` | 其余 3 个服务 |
|---|---|---|
| `protoc-gen-go` | v1.26.0 | v1.27.1 |
| 文件组织 | **单文件**（消息 + gRPC 混在一起） | **拆分**（`_grpc.pb.go` 独立文件） |
| gRPC 版本标记 | `SupportPackageIsVersion6` | `SupportPackageIsVersion7` |
| 服务端接口要求 | 无 | 需嵌入 `UnimplementedXxxServer` |

**这正是「改了 `.proto` 却忘记重新生成」的温床**——项目里并存两套工具链，
开发者自己也不确定该用哪条命令、哪个版本的插件，很容易漏掉重新生成这一步。

### 修复方案

**① 统一迁移到现代工具链**（使 order 与其余 3 个服务一致）

> 注：[deploy/script/gencode/gen.sh](deploy/script/gencode/gen.sh) 里记录的是**老式单文件**的生成命令
> （`goctl rpc protoc ... --zrpc_out=../`），它还会连带重新生成整个服务骨架，不适合本次修复。
> 因此改用等价的现代命令，只重新生成 pb 层。

在 `app/order/cmd/rpc/pb` 目录下执行：

```bash
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       order.proto
```

生成物由原来的单文件变为两个文件：

- `order.pb.go` —— 消息类型（本次关键：新增了字段号 27 的 `NeedFood`）
- `order_grpc.pb.go` —— gRPC 服务代码（新增文件）

**② 修复迁移引发的编译错误**

新版 `protoc-gen-go-grpc` 生成的 `OrderServer` 接口强制要求实现
`mustEmbedUnimplementedOrderServer()`。这是**向前兼容机制**：将来 proto 新增方法时，
未同步更新的服务端实现不会编译失败（会由嵌入的 `Unimplemented` 提供默认实现并返回 `codes.Unimplemented`）。

因此在 [app/order/cmd/rpc/internal/server/orderServer.go](app/order/cmd/rpc/internal/server/orderServer.go)
补充一行嵌入（与其余 3 个服务的 server 写法保持一致）：

```go
type OrderServer struct {
	svcCtx *svc.ServiceContext
	pb.UnimplementedOrderServer
}
```

**③ 确认影响范围**

编写脚本比对全部 4 个 proto 与对应 `.pb.go` 的最大字段号，确认**仅 order 不同步**
（其余 3 个完全一致），因此本次改动只影响 order 服务，不会波及其他服务。

### 验证

**① 编译验证**

```
$ go build ./...
[exit=0]
```

**② 最小复现验证**（精确复现 Bug 根因，不依赖 MySQL / Redis / Kafka）

为了让验证不受环境干扰，我写了一个最小复现程序，直接调用出问题的那一步
（rpc 层 `copier.Copy(model → pb)`）：

```
$ go run ./data/pbverify

=== 验证 1：pb 的 descriptor 里是否包含字段号 27 ===
  ✅ 字段号 27 存在 → name=needFood kind=int64

=== 验证 2：复现 copier.Copy(model → pb)，即 Bug 发生的那一步 ===
  model.HomestayOrder.NeedFood = 1
  pb.HomestayOrder.NeedFood    = 1
  ✅ 字段成功传递

=== 验证 3：protobuf 序列化往返是否保留该字段 ===
  序列化后大小 = 57 字节
  往返后 NeedFood = 1
  ✅ 序列化往返正常

=== 验证 4：GetNeedFood() 访问器存在且返回正确 ===
  GetNeedFood() = 1
  ✅ 访问器正常

🎉 全部验证通过：needFood 字段已修复（修复前验证 1/2/3/4 都会失败）
```

**③ 端到端验证**（Docker 环境，通过 nginx 网关调用真实接口）

数据库事实基准：

```
mysql> SELECT sn, user_id, need_food, trade_state
       FROM looklook_order.homestay_order WHERE need_food = 1;
+---------------------------+---------+-----------+-------------+
| sn                        | user_id | need_food | trade_state |
+---------------------------+---------+-----------+-------------+
| HSO2026100217125693042474 |       3 |         1 |          -1 |
| HSO2026100419080564475151 |       1 |         1 |          -1 |
+---------------------------+---------+-----------+-------------+
```

调用订单详情接口：

```
POST /order/v1/homestayOrder/userHomestayOrderDetail
{"sn":"HSO2026100217125693042474"}

HTTP 200
{
  "needFood": 1,              <-- 修复前恒为 0
  "orderTotalPrice": 1120,
  "tradeState": -1
}
```

**结论**：接口返回的 `needFood` 与数据库中的 `need_food` 一致（均为 `1`），字段不再丢失。

### commit

`77a4a1e` — fix(order): regenerate gRPC code to fix silently dropped needFood field
