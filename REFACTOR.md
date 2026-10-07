# 改造记录（REFACTOR）

本仓库 fork 自 [Mikaelemmmm/go-zero-looklook](https://github.com/Mikaelemmmm/go-zero-looklook)（MIT License）。
我在研读其源码的基础上，对其做缺陷修复、分布式一致性加固、安全加固与可观测性改造。

---

## 改造进度

| # | 改造项 | 状态 | 对应 commit |
|---|---|---|---|
| 1 | 修复 gRPC 代码生成漂移导致字段静默丢失 | ✅ 已完成 | `77a4a1e` |
| 2 | 安全加固 · 日志脱敏：移除日志中的明文密码 | ✅ 已完成 | `506ecc8` |
| 3 | 安全加固 · 密码存储改用 bcrypt | ✅ 已完成 | `58d7fe3` |
| 4 | 安全加固 · JWT 缩短有效期 + jti 黑名单 | 📋 计划中 | — |
| 5 | 修复错误日志格式符未替换（`logx.Error` 误用为 `logx.Errorf`） | 📋 计划中 | — |
| 6 | 引入事务性发件箱（Outbox）解决支付链路双写不一致 | ✅ 已完成 | `a9b1e4b` |
| 7 | 可观测性：修复失效的日志过滤规则、为 ES 配置 Ingest Pipeline | 📋 计划中 | — |
| 8 | **新功能：优惠券**（领券 / 下单抵扣 / 支付核销 / 取消释放） | ✅ 已完成 | `5f7cb86` |

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

---

## 2. 安全加固 · 日志脱敏：移除日志中的明文密码

> 状态：✅ **已完成** —— 已通过 ES 日志前后对比验证

### 问题现象

用户**登录失败**时，输入的**明文密码会被写入应用日志**。从日志平台（ES）检索到的原始记录：

```json
{
  "@timestamp": "2026-10-07T08:35:12.699+08:00",
  "caller": "clientinterceptors/durationinterceptor.go:35",
  "content": "fail - direct:/127.0.0.1:2004/pb.usercenter/login - authType:\"system\"  authKey:\"18116427072\"  password:\"620WFwf0aaa\" - rpc error: code = Code(100001) desc = 账号或密码不正确",
  "level": "error"
}
```

其中 `password:"620WFwf0aaa"` 即用户输入的明文密码。

**危害**：日志会被长期留存并采集到 ES。若有人对系统做密码爆破，**所有尝试过的密码都会被完整记录下来**；
日志平台权限管控不严时，等于泄露一份「用户真实用过的密码候选表」。这也是等保 / PCI-DSS 明确要求的
——**认证凭据不得落盘**。

### 泄漏来源（共两处，必须同时修复）

**来源 A：项目代码用 `%+v` 打印了整个请求体**

[app/usercenter/cmd/api/internal/logic/user/registerLogic.go](app/usercenter/cmd/api/internal/logic/user/registerLogic.go)：

```go
if err != nil {
    return nil, errors.Wrapf(err, "req: %+v", req)   // req 含 Password 字段
}
```

`%+v` 输出 `{Mobile:... Password:...}`，该错误消息最终由 `result.HttpResult` 以 `%+v` 写入日志。

> 已扫描全项目 10 处「用 `%+v` 打印请求体」的代码，**只有这一处含密码**；
> rpc 侧业务逻辑只用 `mobile:%s` / `id:%d` 这类单字段，无泄漏。

**来源 B：go-zero 客户端拦截器会自动打印整个请求结构体**

`zrpc` 默认挂载的 `DurationInterceptor` 在 **RPC 失败**或**触发慢调用阈值**时会把 `req` 整个写进日志：

```go
_, ok := notLoggingContentMethods.Load(method)
if ok {
    logger.Errorf("fail - %s - %s", serverName, err.Error())            // 不打印请求体
} else {
    logger.Errorf("fail - %s - %v - %s", serverName, req, err.Error())   // ★ 打印整个 req
}
```

因此 `LoginReq{password}` / `RegisterReq{password}` 被完整记录。

### 修复方案

**修复 A：只记录定位问题所需的非敏感字段**

```go
if err != nil {
    // 不要把整个 req 打进错误：它包含明文密码，会被写入日志。
    // 只记录定位问题所需的非敏感字段。
    return nil, errors.Wrapf(err, "mobile: %s", req.Mobile)
}
```

**修复 B：把敏感 RPC 方法加入「不记录请求体」名单**

go-zero 已内置该开关（`zrpc.DontLogClientContentForMethod`），
但**必须在发起调用的进程（usercenter-api）中注册，而不是 rpc 服务端**：

[app/usercenter/cmd/api/internal/svc/serviceContext.go](app/usercenter/cmd/api/internal/svc/serviceContext.go)

```go
func NewServiceContext(c config.Config) *ServiceContext {
	// 关闭 go-zero 客户端拦截器对敏感方法请求体的日志输出。
	// 方法名格式为 /{proto包名}.{服务名}/{方法名}；
	// 本项目 proto 中服务名与方法名均为小写（见 app/usercenter/cmd/rpc/pb/usercenter.proto）。
	zrpc.DontLogClientContentForMethod("/pb.usercenter/login")
	zrpc.DontLogClientContentForMethod("/pb.usercenter/register")

	return &ServiceContext{ ... }
}
```

> **两个易错点（本次都遇到并确认）**：
>
> 1. **方法名必须与 `.proto` 逐字一致。** 本项目 proto 中写的是小写
>    （`package pb; service usercenter { rpc login(...) }`），所以是 `/pb.usercenter/login`，
>    **不是** `/pb.Usercenter/Login`。go-zero 官方单元测试的格式参考：
>    `DontLogContentForMethod("/foo")`（**带前导斜杠**）。
> 2. **必须放在 api 进程。** 泄漏发生在「客户端拦截器」，即 rpc 的**调用方**（usercenter-api）。
>    若放到 rpc 的 serviceContext 里不会生效。

### 验证

**方法**：通过 nginx 网关发起「密码错误」的登录请求，再从 ES 日志平台检索前后变化。

**改前**（08:30 ~ 08:35，共 4 条）：

```json
{"caller":"clientinterceptors/durationinterceptor.go:35",
 "content":"fail - direct:/127.0.0.1:2004/pb.usercenter/login - authType:\"system\"  authKey:\"18116427072\"  password:\"620WFwf0aaa\" - rpc error: code = Code(100001) desc = 账号或密码不正确"}
```

**改后**（08:39 起）：

```json
{"caller":"clientinterceptors/durationinterceptor.go:33",
 "content":"fail - direct:/127.0.0.1:2004/pb.usercenter/login - rpc error: code = Code(100001) desc = 账号或密码不正确"}
```

**两处结构性差异（这才是强证据）：**

| | 改前 | 改后 |
|---|---|---|
| `caller` 行号 | `durationinterceptor.go:35` | `durationinterceptor.go:`**`33`** |
| 日志内容 | 含 `authType` / `authKey` / `password` | **请求体整段消失** |

> **为什么行号变化是强证据？** 因为程序走了另一条分支：
> `:35` 是「打印请求体」的分支，`:33` 是「不打印请求体」的分支。
> **行号变化证明是代码路径变了，而不是"日志恰好没刷出来"。**

**功能未受影响**：
- 正确密码登录仍正常返回 `{"code":200,"msg":"OK","data":{"accessToken":"..."}}`
- 错误码与错误消息保持不变（`100001` / `账号或密码不正确`）

### 排查过程中的一个坑（值得记录）

第一次验证时密码**仍然出现**在日志中。原因不是修复方案有问题，而是
**modd 没有重新编译成功（编译失败是静默的）**，容器里运行的仍是旧二进制。

排查方法：

```powershell
# 1) 确认容器内看到的文件是不是最新的
docker exec looklook cat /go/looklook/app/usercenter/cmd/api/internal/svc/serviceContext.go

# 2) 确认二进制编译时间
docker exec looklook ls -l --time-style=full-iso /go/looklook/data/server/usercenter-api

# 3) 查看是否有编译错误
docker logs looklook --tail 80 2>&1 | Select-String 'cannot|undefined|error|failed'

# 4) 强制重新编译
docker restart looklook
```

**教训**：**改了代码 ≠ 运行的是新代码。** 遇到「修了但没生效」，应先确认
「当前运行的产物是哪一次构建的」，而不是怀疑修复方案本身。
这与 CI/CD 中「构建失败但仍在跑旧版本」是同一类问题。

### commit

`506ecc8` — fix(usercenter): stop writing plaintext passwords to logs

---

## 3. 安全加固 · 密码存储改用 bcrypt

> 状态：✅ **已完成** —— 通过渐进式迁移验证、加盐验证、日志无泄漏三层验证

### 问题现象

密码使用**无盐 MD5** 存储（`tool.Md5ByString`），存在两个致命缺陷。

**缺陷 1：无盐 → 相同密码产生相同哈希**

数据库中 **17 个用户共用同一个哈希**：

```
| id | mobile      | password                         |
| 6  | 13766660001 | 900150983cd24fb0d6963f7d28e17f72 |  ┐
| 7  | 13766660002 | 900150983cd24fb0d6963f7d28e17f72 |  ├ 17 个账号密码完全相同
| .. | ...         | 900150983cd24fb0d6963f7d28e17f72 |  │
| 25 | 13766660020 | 900150983cd24fb0d6963f7d28e17f72 |  ┘
```

攻击者无需破解即可判定这些账号密码相同 —— **破一个等于破 17 个**。

**缺陷 2：MD5 计算过快 → 字典/彩虹表可瞬间反查**

仅用 8 个常见候选密码计算 MD5 与库中值比对，即可还原 **20 / 25（80%）** 的密码：

| 库中哈希 | 还原出的明文 | 涉及用户数 |
|---|---|---|
| `e10adc3949ba59abbe56e057f20f883e` | `123456` | 1 |
| `e99a18c428cb38d5f260853678922e03` | `abc123` | 2 |
| `900150983cd24fb0d6963f7d28e17f72` | `abc` | 17 |

### 设计难题：存量数据无法批量迁移

数据库里已有 25 个用户存的是 32 位 MD5 哈希。**若直接把校验改成 bcrypt，这些用户将永远无法登录** ——
因为 bcrypt 无法验证一个 MD5 哈希。

**而 MD5 是单向的**（`明文 → MD5` 可行，`MD5 → 明文` 不可行），因此**无法提前把存量哈希批量转换为 bcrypt**
—— 服务端根本不知道用户的明文密码。

> 这是密码算法升级的通用难题：**只能等用户下次登录、拿到明文的那一刻，才有机会迁移。**

### 方案对比

| 方案 | 做法 | 用户体验 | 代码复杂度 | 是否采纳 |
|---|---|---|---|---|
| **A. 渐进式迁移** | 登录时判断存储值格式：bcrypt 则用 bcrypt 校验；MD5 则用 MD5 校验，**通过后立即改存 bcrypt** | 完全无感 | 多一个分支 | ✅ **采纳** |
| B. 双字段 | 新增 `password_v2` 列，新数据写 bcrypt | 无感 | 需改表结构，逻辑分散 | ❌ |
| C. 强制重置 | 清空所有密码哈希 | 很差 | 最简单 | ❌ |

**选择 A 的理由**：不改表结构、用户无感、迁移幂等、老用户登录一次即自动完成升级。

### 实现

**① `pkg/tool/encryption.go`：新增 bcrypt 工具函数**

> `Md5ByString` **保留不动** —— 存量用户的迁移分支仍需要它。

```go
// HashPassword 使用 bcrypt 生成密码哈希。
// bcrypt 会自动生成随机盐并编码进哈希串，因此同一密码每次生成的结果都不同。
// 注意：bcrypt 不支持超过 72 字节的密码，超长会返回错误。
func HashPassword(password string) (string, error) {
	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

// CheckPassword 校验明文密码与 bcrypt 哈希是否匹配。
func CheckPassword(hashedPassword, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hashedPassword), []byte(password)) == nil
}

// IsBcryptHash 判断存储的哈希是否为 bcrypt 格式，用于存量 MD5 数据的渐进式迁移。
// bcrypt 哈希固定 60 字符，形如 $2a$10$N9qo8uLOickgx2ZMRZoMye...
func IsBcryptHash(hash string) bool {
	return len(hash) == 60 && strings.HasPrefix(hash, "$2")
}
```

**② 注册改为 bcrypt**（[registerLogic.go](app/usercenter/cmd/rpc/internal/logic/registerLogic.go)）

```go
if len(in.Password) > 0 {
	hashed, hashErr := tool.HashPassword(in.Password)
	if hashErr != nil {
		return errors.Wrapf(xerr.NewErrCode(xerr.SERVER_COMMON_ERROR), "bcrypt hash password err:%v", hashErr)
	}
	user.Password = hashed
}
```

**③ 登录实现渐进式迁移**（[loginLogic.go](app/usercenter/cmd/rpc/internal/logic/loginLogic.go)）

```go
if !l.verifyAndMigratePassword(user, password) {
	return 0, errors.Wrap(ErrUsernamePwdError, "密码匹配出错")
}
```

```go
// verifyAndMigratePassword 校验密码，并对存量 MD5 哈希做渐进式迁移（Lazy Migration）。
//
// ⚠️ 安全约束：本函数内不得记录 password 的明文或任何哈希值（详见第 2 项改造）。
func (l *LoginLogic) verifyAndMigratePassword(user *model.User, password string) bool {
	// 分支一：新数据，直接用 bcrypt 校验
	if tool.IsBcryptHash(user.Password) {
		return tool.CheckPassword(user.Password, password)
	}

	// 分支二：存量 MD5 数据
	if tool.Md5ByString(password) != user.Password {
		return false
	}

	// 密码校验已通过 → 就地升级为 bcrypt
	hashed, err := tool.HashPassword(password)
	if err != nil {
		// 升级失败不影响本次登录（密码本身是正确的），下次登录会重试
		l.Errorf("migrate password to bcrypt failed, userId=%d, err=%v", user.Id, err)
		return true
	}

	user.Password = hashed
	if _, err := l.svcCtx.UserModel.Update(l.ctx, nil, user); err != nil {
		l.Errorf("update bcrypt password failed, userId=%d, err=%v", user.Id, err)
	}
	return true
}
```

### 验证

**① 渐进式迁移前后对比**（用户 3：`13777778888` / 密码 `abc123`）

```
迁移前:  id=3  len=32  prefix=e99a18c        ← 老 MD5（e99a18c... = MD5("abc123")）
登录:    code=200  msg=OK  (token 147 字符)   ← 老密码仍可正常登录
迁移后:  id=3  len=60  prefix=$2a$10$        ← ★ 已自动升级为 bcrypt
再登录:  code=200  msg=OK                    ← 走 bcrypt 分支，幂等
```

**② 加盐验证：相同密码 → 不同哈希**

注册两个账号，使用**完全相同的密码** `SAMEPWD-CHECK`：

```
id=31  13922220001  $2a$10$oSM5nCjt73xq/BM8GLhf/OFMEDJseJU/D/.LWYrB1AU/PHXvPn9R.
id=32  13922220002  $2a$10$z5W0wCTo8qX19JvL/beN2eXH/8qSmAzzzi3PaG2O.KfEu7ADYmo6e
                    ↑ 密码完全相同，但哈希【不同】= 随机盐生效
```

**对比**：同一密码在旧方案下两者都会是 `16ead4dfad4c248ad96c44cc24ece371`（**一模一样**）。

**③ 日志无泄漏**（守住第 2 项改造的成果）

在 ES 中检索本次测试使用的明文密码：

```
POST /looklook-*/_search  {"query":{"match_phrase":{"data.log":"SAMEPWD-CHECK"}}}
  命中数: 0   ✅

最近 20 分钟内检索 "abc123"：命中数 0   ✅
登录失败日志仍走「不记录请求体」分支：caller = durationinterceptor.go:33   ✅
```

### 关键设计点

1. **升级失败不得影响登录**：密码已校验通过，若此时因写库失败就返回登录失败，用户会遇到莫名其妙的错误。
   因此只记录日志并照常返回成功，下次登录会自动重试。
2. **迁移是幂等的**：首次登录完成升级，再次登录走 bcrypt 分支；即便升级那一步失败，下次登录仍会重试，不会卡在中间状态。
3. **⚠️ 迁移路径上绝不能记录明文密码**：`明文 → MD5 比对 → 通过 → bcrypt 加密` 全程明文在内存中。
   若在此处加一行 `logx.Infof("...%s", password)`，就会在**登录成功路径**上新建一个明文泄漏点，
   使第 2 项改造前功尽弃。故此处只记录 `userId`。
4. **bcrypt 的 72 字节上限**：`GenerateFromPassword` 对超过 72 字节的密码返回错误，必须处理该错误，
   **绝不能静默截断**（截断会削弱安全性，并造成「前 72 字节相同即可登录」的隐患）。
5. **`Md5ByString` 暂不可删**：只要仍有用户停留在 MD5 分支，迁移代码就必须保留。
   后续可通过日志统计 MD5 分支命中情况，确认长时间为 0 后再发一次提交移除旧算法。

### commit

`58d7fe3` — feat(usercenter): migrate password storage from MD5 to bcrypt

---

## 6. 引入事务性发件箱（Outbox）解决支付链路双写不一致

> 状态：✅ **已完成** —— 通过端到端投递 / 消费端幂等 / 退避重试 / 事务原子性 / Kafka 宕机不丢 五层验证

### 问题现象

[app/payment/cmd/rpc/internal/logic/updateTradeStateLogic.go](app/payment/cmd/rpc/internal/logic/updateTradeStateLogic.go)
对两个独立系统做了两次写入：

```go
// ③ 写 MySQL
if err := l.svcCtx.ThirdPaymentModel.UpdateWithVersion(l.ctx, nil, thirdPayment); err != nil {
	return nil, ...
}

// ④ 发 Kafka
if err := l.pubKqPaySuccess(in.Sn, in.PayStatus); err != nil {
	logx.WithContext(l.ctx).Errorf("l.pubKqPaySuccess : %+v", err)   // ← 只记日志
}
```

两者之间没有任何原子性保证：

```
③ 成功、④ 失败  →  支付流水已更新，但订单服务收不到通知
                    ★ 用户付了钱，订单仍是「待支付」
③ 失败          →  不会走到 ④，一致 ✓
```

更糟的是：④ 的失败**只记录日志、接口照常返回成功**，没有任何人知道出了问题。

### 更深的问题：`kq.Pusher` 的错误处理几乎是装饰性的

读 go-queue v1.1.8 源码（`kq/pusher.go`）发现三处问题：

```go
func (p *Pusher) Push(v string) error {
	msg := kafka.Message{
		Key:   []byte(strconv.FormatInt(time.Now().UnixNano(), 10)),  // ③ 时间戳当 key
		Value: []byte(v),
	}
	if p.executor != nil {
		return p.executor.Add(msg, len(v))     // ① 只是放进缓冲区
	}
	...
}

// ② 真实发送错误在这里被吞掉，调用方完全不知道
pusher.executor = executors.NewChunkExecutor(func(tasks []interface{}) {
	if err := pusher.produer.WriteMessages(context.Background(), chunk...); err != nil {
		logx.Error(err)
	}
})
```

| # | 问题 | 后果 |
|---|---|---|
| ① | `Push` 是**缓冲的**，返回 nil ≠ 消息已发出 | 业务里的 `if err != nil` **几乎永不触发** |
| ② | 真实发送错误被 `logx.Error` 吞掉 | 业务完全不知道消息丢了 |
| ③ | Kafka key 用**纳秒时间戳** | 同一订单的消息散落到不同分区 → **乱序** |

### 为什么不能"加个事务"

MySQL 与 Kafka 是两个独立系统，**没有跨系统的本地事务**。
2PC 不可行（Kafka 不支持 XA，且协调者单点）；Kafka 也没有 RocketMQ 的事务消息（半消息）机制。

### 方案对比与选择

| 方案 | 说明 | 是否采纳 |
|---|---|---|
| 尽力而为（原实现） | 先写库再发消息，失败只记日志 | ❌ 会丢消息 |
| 2PC / XA | 分布式事务 | ❌ Kafka 不支持；性能差；协调者单点 |
| 事务消息 | MQ 提供半消息机制 | ❌ Kafka 没有该能力（RocketMQ 才有） |
| CDC（Debezium 读 binlog） | 从数据库变更日志捕获 | ❌ 需额外引入组件，本项目无此基础设施 |
| **事务性发件箱（Outbox）** | 业务数据与待发消息在同一本地事务落地 | ✅ **采纳** |

### 核心思想

> **不要「写库 + 发消息」，而要「写库 + 写一条待发消息」** ——
> 因为后者**两个都是数据库操作**，可以放进同一个本地事务。

```
┌────────── 同一个本地事务（原子） ──────────┐
│  ① UPDATE third_payment ...               │
│  ② INSERT INTO outbox (topic,key,payload) │
└───────────────────────────────────────────┘
         ↓ 提交：要么都成功，要么都回滚
   （此刻消息已安全落地，只是还没发出去）
         ↓
   ③ relay 扫描 outbox → 投递 Kafka → 标记已发送
         ↓
   ④ Kafka → order-mq 消费 → order-rpc 更新订单状态（幂等）
```

**三个保证：**

| 保证 | 靠什么实现 |
|---|---|
| **不丢（原子性）** | 业务数据与 outbox 记录在同一本地事务 → 不存在「改了库但没记消息」 |
| **不丢（持久性）** | Kafka / relay 挂掉时消息仍在表中 → relay 恢复后重投 |
| **不重（幂等）** | relay 可能「投递成功但标记前崩溃」→ 重启后重投 → **消费端必须幂等** |

### 表结构

```sql
CREATE TABLE `outbox` (
  `id`            bigint        NOT NULL AUTO_INCREMENT,
  `topic`         varchar(64)   NOT NULL DEFAULT '',
  `msg_key`       varchar(64)   NOT NULL DEFAULT '',
  `payload`       varchar(2048) NOT NULL DEFAULT '',
  `status`        tinyint       NOT NULL DEFAULT 0 COMMENT '0待投递 1已投递 2已死信',
  `retry_count`   int           NOT NULL DEFAULT 0,
  `next_retry_at` datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `last_error`    varchar(512)  NOT NULL DEFAULT '',
  `created_at`    datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `sent_at`       datetime      DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_status_next_retry` (`status`,`next_retry_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='事务性发件箱';
```

**设计说明：**

| 字段 | 作用 |
|---|---|
| `msg_key` | 存业务唯一键（订单号），投递时作为 Kafka key → **同一订单的消息落同一分区 → 有序** |
| `status` | `0 待投递` / `1 已投递` / `2 死信`（重试超限，需人工介入） |
| `retry_count` + `next_retry_at` | **指数退避**，避免失败时疯狂重试拖垮下游 |
| `last_error` | 记录失败原因，便于排查 |
| `idx_status_next_retry` | 正好覆盖 relay 的核心查询 `WHERE status=0 AND next_retry_at<=now()` |

### 实现

**① 业务改造：两个写入放进同一个事务**

```go
err = l.svcCtx.OutboxModel.Trans(l.ctx, func(ctx context.Context, session sqlx.Session) error {
	// ① 更新业务数据（传入同一个 session）
	if err := l.svcCtx.ThirdPaymentModel.UpdateWithVersion(ctx, session, thirdPayment); err != nil {
		return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "...", err)
	}

	// ② 写待发消息（与①同事务 → 原子）
	body, err := json.Marshal(kqueue.ThirdPaymentUpdatePayStatusNotifyMessage{
		OrderSn:   in.Sn,
		PayStatus: in.PayStatus,
	})
	if err != nil {
		return errors.Wrapf(xerr.NewErrMsg("outbox payload marshal error"), "...", err)
	}

	if _, err = l.svcCtx.OutboxModel.Insert(ctx, session, &model.Outbox{
		Topic:   l.svcCtx.Config.KqPaymentUpdatePayStatusConf.Topic,
		MsgKey:  in.Sn, // ★ 用订单号作 Kafka key，保证同一订单有序
		Payload: string(body),
		Status:  model.OutboxStatusPending,
	}); err != nil {
		return errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "...", err)
	}
	return nil
})
if err != nil {
	return nil, err
}
```

**关键变化**：以前发消息失败只记日志，现在**任何一步失败都会回滚整个事务并返回错误**。
同时删除了原有的 `pubKqPaySuccess` 直发路径（见踩坑记录第 7 条）。

**② relay 服务**（[app/payment/cmd/relay/](app/payment/cmd/relay/)）

刻意**不使用 `kq.Pusher`**，改用底层的 `github.com/segmentio/kafka-go`：

```go
writer := &kafka.Writer{
	Addr:         kafka.TCP(c.Brokers...),
	Topic:        c.Topic,
	Balancer:     &kafka.Hash{},        // 按 key 分区 → 同一订单消息有序
	RequiredAcks: kafka.RequireAll,     // 等所有 ISR 确认才算成功
	Async:        false,                // 同步发送，才能拿到真实错误
}
```

**③ 并发安全：`FOR UPDATE SKIP LOCKED`**

```sql
select ... from outbox
where status = 0 and next_retry_at <= ?
order by id asc limit ?
for update skip locked
```

- `FOR UPDATE` 锁住取到的行
- `SKIP LOCKED` **跳过已被其他事务锁住的行** → 多个 relay 实例各取各的，
  **既不重复投递、也不互相阻塞**（MySQL 8.0+ 特性）

### 验证

**① 端到端投递**

```
11:31:25  订单创建           trade_state = 0
11:31:30  插入 outbox        status=0，未被投递
11:31:32  relay 投递成功     status=1，sent_at=11:31:32
11:31:32  订单状态变更        trade_state 0 → 1
```

链路：`outbox(status=0)` → relay（每 500ms 扫描）→ Kafka（key=订单号，acks=all）→
order-mq 消费 → order-rpc 状态机 → 订单变为「待使用」。**延迟 2 秒。**

**② 消费端幂等**（模拟「投递成功但标记 sent 前崩溃」）

```
原 update_time = 11:31:32
把 outbox 记录改回 status=0 → relay 重投（sent_at=11:31:46，确实重投了）
重投后 update_time = 11:31:32   ★ 完全没变
```

`update_time` 未变，说明第二次消息**没有触发数据库 UPDATE** ——
[updateHomestayOrderTradeStateLogic.go](app/order/cmd/rpc/internal/logic/updateHomestayOrderTradeStateLogic.go)
中「状态相同则提前返回」的幂等逻辑生效。

**③ 退避门控 + 恢复投递**

```
11:32:10  插入记录（next_retry_at = 11:32:25，retry_count=3，模拟已失败 3 次）
11:32:14  检查 → status 仍为 0（被退避门控挡住，未投递）订单仍为 0
11:32:26  到期后自动投递 → status=1  订单 0 → 1
```

**证明退避门控生效，且到期后无需人工干预即可自动恢复投递。**

**④ 事务原子性**（故障注入）

在一个事务内：先用真实的 `UpdateWithVersion(ctx, session, ...)` 更新业务数据，
再故意插入一条 `msg_key` 超长（100 字符 > `varchar(64)`）的 outbox 记录使其失败：

```
1. SELECT third_payment       → trade_state="ORIGINAL_STATE", version=0
2. UPDATE third_payment SET trade_state='SHOULD_NOT_PERSIST' WHERE id=42 AND version=0
3. INSERT INTO outbox (msg_key=100个X)
   → Error 1406 (22001): Data too long for column 'msg_key'
4. 事务回滚
5. 重新 SELECT                → trade_state="ORIGINAL_STATE", version=0   ★ 完全没变
```

**证明两个写入确实同生共死**：outbox 插入失败 → 业务数据的更新被回滚。

**⑤ Kafka 宕机不丢消息**

```
① docker stop kafka
② 插入一条待投递消息
③ 观察：status 保持 0，retry_count 递增，last_error 记录连接错误；订单仍为「待支付」
④ docker start kafka
⑤ 观察：消息最终被投出，status=1，订单 0 → 1
```

**这是 Outbox 最核心的承诺：Kafka 不可用期间消息不丢，恢复后自动补投。**

### 关键设计点

1. **消息只保留一条路径**：改造后业务代码只写 outbox，不再直发 Kafka。
   若同时保留两条路径会产生重复消息（比丢失更难排查）。
2. **消费端幂等是设计的一部分**，不是可选项 —— Outbox 天然是 at-least-once 语义。
   本项目消费端的状态机已天然满足（状态相同则提前返回）。
3. **死信机制**：重试超限（默认 10 次）转 `status=2`，不再无限重试拖累队列，需告警/人工介入。
4. **定期清理**：relay 每小时删除 7 天前已投递成功的记录，避免表无限膨胀。
5. **⚠️ 已知权衡**：relay 在事务内做网络 I/O，会持有行锁。
   低并发下可接受；生产级优化是**先在短事务内把记录标记为「发送中」并记录认领时间，
   再在事务外投递**，配合超时回收机制。

### commit

`a9b1e4b` — feat(payment): introduce transactional outbox for pay-status notification

---

## 8. 新功能：优惠券（领券 / 下单抵扣 / 支付核销 / 取消释放）

> 状态：✅ **已完成** —— 完整业务闭环已验证
>
> 这是一次完整的「从零开发」：需求 → 领域建模 → 数据库设计 → API 契约 → 代码生成 → 实现 → 验证。
> 设计文档（含 15 条决策记录 D1~D15 与 20 个设计问答）见 **[doc/coupon-design.md](doc/coupon-design.md)**。

### 需求

运营配置券模板 → 用户领券 → 下单时抵扣 → 支付后核销 → 取消/超时释放 → 到期回收。

**明确不做**（控制范围）：后台管理界面 / 券叠加 / 转赠 / 秒杀 / 退款退券 / 多业务类型 / 到期提醒。

### 领域建模

**状态机（`user_coupon.status`）：**

```
                        ┌──── 订单取消 / 超时释放 ────┐
                        │  CAS: 1 → 0，清空 order_sn │
                        ▼                            │
  ┌───────────┐   锁券(下单)   ┌───────────┐  支付成功  ┌───────────┐
  │ 0 未使用  │ ────────────> │ 1 已锁定  │ ────────> │ 2 已核销  │
  └───────────┘   CAS: 0 → 1  └───────────┘  CAS: 1→2  └───────────┘
        │                                                  （终态）
        │ 到期（定时任务）CAS: 0 → 3
        ▼
  ┌───────────┐
  │ 3 已过期  │  （终态）
  └───────────┘
```

**三张表**：`coupon_template`（券模板）、`user_coupon`（用户券）、`coupon_use_record`（操作流水，审计）。

### 实现

- **独立微服务** `app/coupon/`（api + rpc）+ 独立库 `looklook_coupon`
- **model 层不使用缓存** —— 券状态是强一致要求，且状态迁移全走自定义 CAS SQL（见设计文档 D11）
- **优惠信息在领取时快照**到用户券（`discount_amount` / `min_amount` / `expire_time`），
  避免运营改模板导致已发出的券缩水（D10）
- **跨服务集成**：`order-rpc` 下单时调 `LockCoupon`；订单状态变更时调 `UseCoupon` / `ReleaseCoupon`

### 四个关键技术点

#### ① 库存防超发 + 防重复使用 —— 用数据库原子 CAS，不用分布式锁

```sql
-- 扣库存：把「判断是否领完」和「递增」合并成一条 SQL，InnoDB 行锁天然串行化
UPDATE coupon_template SET issued_count = issued_count + 1
WHERE id = ? AND issued_count < total_count;

-- 锁券：WHERE status = 0 是「比较」，SET status = 1 是「交换」
UPDATE user_coupon SET status = 1, order_sn = ?, lock_time = NOW()
WHERE id = ? AND user_id = ? AND status = 0;
```

**为什么不用分布式锁**：能用一条数据库原子语句解决的，不必引入 Redis 锁的超时、续期、误删他人锁等问题。
（`user_id` 刻意放在 `WHERE` 里 —— 让「越权使用别人的券」在**数据库层**就不可能发生。）

> ⚠️ 注意区分：`FOR UPDATE SKIP LOCKED`（我们在 Outbox relay 扫描里用的）是**任务抢占**模式 ——
> "谁抢到算谁的，抢不到的跳过"。而这里是**资源竞争**，抢不到必须**明确报错告诉用户**，不能静默跳过。

#### ② 限领校验的 TOCTOU 竞态与修复

最初把限领校验放在**事务外**，并发下会超领：

```
请求 A: count=0 → 通过  ┐
请求 B: count=0 → 通过  ├ 都读到 0，然后各自插一张券 → 同一用户领到 2 张
```

**修复的三个要点（缺一不可）**：

| # | 做法 | 为什么 |
|---|---|---|
| 1 | 校验**移进事务** | 与发券操作原子 |
| 2 | 放在**扣库存之后** | `IncrIssuedCount` 锁住模板行 → **同一模板的并发请求被串行化** |
| 3 | 用 `SELECT ... FOR UPDATE`（**当前读**） | RR 隔离级别下普通 SELECT 是**快照读**，看不到并发事务刚提交的插入 |

**校验失败直接返回错误 → 事务回滚 → 刚扣的库存自动退回，无需补偿代码。**

#### ③ 幂等的统一实现模式

`LockCoupon` / `UseCoupon` / `ReleaseCoupon` 共用同一套写法：

```
1. CAS 更新（WHERE 带状态条件）
2. 影响行数 == 1 → 成功
3. 影响行数 == 0 → **不直接报错**，回查状态：
     · 已是「目标状态」 → 幂等返回成功
     · 是别的状态       → 真正的错误
```

**为什么必须幂等**：跨服务调用超时时，调用方**无法判断到底成功了没有**。
只有被调方幂等，调用方才敢重试；否则只能"不重试"（可能漏）或"重试"（可能重）。

`LockCoupon` 额外多一次判断：区分「被我自己的订单锁的」（→ 成功）与「被别人锁的」（→ `COUPON_OCCUPIED_BY_OTHER_ORDER`）。

#### ④ 跨服务一致性的处理顺序与补偿

```
① 参数校验 / 查民宿 / 算价     —— 只读，失败无副作用
② 调 coupon-rpc.LockCoupon    —— 第一个有副作用的操作
③ Insert 订单                 —— 第二个写操作
④ 排延迟关单任务              —— 后续动作
```

**把锁券放在写订单之前**：锁券失败时订单还没建，**直接返回即可，没有任何脏状态需要清理**。
反过来（先写订单再锁券）失败时，要么删订单、要么留一张没券的订单，处理更麻烦。

**② 成功、③ 失败** → 用 `defer` + `success` 标志做**补偿**：

```go
success := false
if couponLocked {
    defer func() {
        if success { return }
        // 补偿：释放券（ReleaseCoupon 本身幂等，重复调用无副作用）
        l.svcCtx.CouponRpc.ReleaseCoupon(releaseCtx, &coupon.ReleaseCouponReq{OrderSn: order.Sn})
    }()
}
// ... Insert 订单 ...
success = true
```

> ⭐ **为什么这套方案可以接受**：即使补偿也失败，**券会在 30 分钟后被延迟任务自动释放** ——
> 这是「用最终一致 + 自愈机制替代强一致」的实用做法。

### 验证

**① 领券**：`{"code":200,"data":{"couponCode":"..."}}`；
重复领取 → `HTTP 400 + {"code":102003,"msg":"您已达到领取上限"}`

**② 并发领券不超领**（10 个并发请求，同一用户、同一模板）

```
coupon_template.issued_count:  0 → 1         ← 只发出 1 张
user_coupon:  user_id=5, template_id=2, cnt=1
```

**③ 预览可用券（门槛过滤 + 金额计算）**

```
订单 15000 分（150元） → 有可用券，finalAmount = 15000 − 2000 = 13000   ✅
订单  5000 分（ 50元） → 空列表（未达 10000 分门槛）                      ✅
```

**④ ⭐ 完整业务闭环（跨三个服务）**

```
① 下单锁券
   券:   status 0 → 1（已锁定），order_sn 关联订单，lock_time=18:39:03
   订单: homestay_total_price=100000 → order_total_price=98000   ✅ 抵扣 2000 分
   流水: action=2（lock）

② 支付成功核销（payment outbox → relay → Kafka → order-mq → order-rpc → coupon-rpc）
   outbox: status=1（投递成功），sent_at=18:41:12
   订单:   trade_state 0 → 1（待使用）
   券:     status 1 → 2（已核销），use_time=18:39:38                ★ 关键证明
   流水:   action=3（use）

③ 幂等（同一条支付消息重复投递）
   订单 update_time = 18:39:38【未被 18:41:12 那次刷新】
   → 命中「状态相同则提前返回」的幂等分支                            ✅
```

### 已知风险（诚实记录）

| # | 风险 | 影响 | 演进方向 |
|---|---|---|---|
| 1 | `LockCoupon` 成功但写订单失败 | 靠 defer 补偿 + 30 分钟自动释放兜底 | Saga 编排，或把锁券纳入订单的本地事务（Outbox 变体） |
| 2 | **订单状态更新成功但 `UseCoupon` 失败** | 券停留在"已锁定"，30 分钟后被自动释放（**用户白用了优惠**） | **把 order 侧也接入 Outbox**（二期）—— 这是又一处跨服务双写 |
| 3 | `FOR UPDATE` 在 RR 下会加 gap lock | 高并发有死锁风险 | 改用 Redis 原子计数，或在入口做幂等键 |
| 4 | 券过期靠定时扫描 | 可能有延迟 | 查询时已做兜底过滤；二期改为领取时排 asynq 延迟任务 |
| 5 | 券名称不做快照（实时读模板） | 模板改名会影响已发出的券的显示 | 可接受（名称是展示信息，不参与金额计算） |
| 6 | 券过期回收的定时任务**尚未实现** | 过期券会一直停留在"未使用" | 查询时已兜底过滤，不影响用户；二期补定时任务 |

### ✅ 上线前代码审查发现的缺陷（已修复）

> 功能跑通、PR 合并之后又做了一次全局审查，发现一个**功能测试覆盖不到**的严重缺陷。
> 记录在此，因为它比"功能能跑"更有价值 —— **能跑通的代码不等于没有 bug**。

#### 缺陷｜`UpdateHomestayOrderTradeState` 的提前返回**跳过了券联动**，导致券永久卡死

```go
// 修复前
if homestayOrder.TradeState == in.TradeState {
    return &pb.UpdateHomestayOrderTradeStateResp{}, nil   // ← 直接返回，券联动被跳过
}
```

**根因**：把「订单状态变更」和「券状态同步」当成一件事做了短路，
但**它们的幂等边界不同** —— 订单的幂等语义是"状态没变就什么都不做"，
而券的同步**本身就是幂等的，恰恰需要被重复执行**。

**触发场景（取消路径最严重）**：

```
订单取消 → ReleaseCoupon 失败（coupon-rpc 抖动 / 网络超时）
   ↓
订单状态已是 Cancel
   ↓
30 分钟后延迟关单任务重跑 → 命中提前返回 → 不释放券
   ↓
🔴 券永久卡在「已锁定」，没有任何机制能恢复
```

**修复**：抽出 `syncCoupon(sn, tradeState)`，在**两条路径上都调用**。
`UseCoupon` / `ReleaseCoupon` 本身幂等，重放绝对安全 ——
**这正是幂等设计的价值：让调用方可以放心重试。**

> **⭐ 教训（可迁移到任何服务）**：
> **「幂等」不是「跳过」的理由。**
> 当一个方法由 A、B 两个独立的写组成，只对 A 做幂等短路，B 就永远失去了补偿机会。
> 正确做法是 **A 按 A 的幂等语义短路，B 按 B 的幂等语义独立执行**。

**验证证据（构造脏状态 → 观察自愈）**：

```
① 造脏状态：订单已是 trade_state=1(待使用)，手动把券改回 status=1(已锁定)
             （模拟"订单更新成功、但 UseCoupon 失败"）

② 重发同一条支付成功消息 → 订单已是目标状态，走「提前返回」分支

③ 结果
   订单 update_time   = 19:37:25  【未变化】→ 证明确实命中了提前返回分支
   券     status      = 1 → 2     【自愈成功】★ 证明修复生效
   流水   新增 action=3(use)      create_time=19:41:30
```

**三行证据互相印证**：`update_time` 未变说明走了短路分支，
而券却完成了核销 —— 只有"短路分支里也执行券联动"才能同时满足这两点。
（修复前跑同样的操作，券会永远停在 `status=1`。）

#### 同步修复的其他问题

| # | 问题 | 修复 |
|---|---|---|
| 2 | `coupon_use_record.action` 的 **DDL 注释与代码常量错位**（注释写 `1锁定 2核销…`，实际是 `1领取 2锁定 3核销 4释放 5过期`） | 修正 `deploy/sql/looklook_coupon.sql`，并 `ALTER TABLE` 同步线上表注释 |
| 3 | 券过期回收的定时任务**未实现** → 过期券长期显示「未使用」 | 已记录为「已知风险 #6」（查询侧已有 `expire_time > now()` 兜底） |
| 4 | `CouponTemplateModel.Trans` 是死代码 | 保留（与 `UserCouponModel` 保持 API 对称） |

#### 审查中确认**没有问题**的关键点

| 检查项 | 结论 |
|---|---|
| **`idx_order_sn` 是否为唯一索引** | 非唯一 ✓ **这是个隐藏大坑**：若为唯一索引，释放券把 `order_sn` 置空后，**第二张被释放的券会因空串重复而更新失败** |
| **MySQL 时区 vs Go 时区** | 两边完全一致 ✓ `NOW()` 与 `time.Now()` 不会打架 |
| **`DB_ERROR`/`SERVER_COMMON_ERROR` 是否在 errMsg 白名单** | 都在 ✓ 所以 `errors.Wrapf(xerr.NewErrCode(...))` 这个项目惯用写法能正确透出消息 |
| **事务内是否全部用 `WithSession`** | ✓ `ClaimCoupon` 的 4 个写操作无遗漏 |
| **锁定的券会不会被误扫成过期** | ✓ `FindExpiredList`/`ExpireById` 都限定 `status=0`，**已锁定的券永远不会被扫成过期** |
| **`gofmt -l` 报 68 个文件** | 环境误报（Windows CRLF），从未改过的 `app/travel` 同样被标记 |
| **越权防护** | ✓ `user_id` 在 `WHERE` 里；越权返回 `NOT_FOUND` 而非"不是你的券"（防券码探测） |

### 上线前的完整测试结果

| # | 测试 | 结果 |
|---|---|---|
| 1 | 领券 / 重复领取拦截 | ✅ 第二次返回 `HTTP 400 + 102003 您已达到领取上限` |
| 2 | **并发 10 次领券（同一用户同一模板）** | ✅ 库存 `0 → 1`，**只发出 1 张** |
| 3 | 预览可用券（门槛过滤 + 金额计算） | ✅ 150 元订单 `finalAmount=13000`；50 元订单返回空 |
| 4 | **并发 10 次下单同一张券** | ✅ **成功 1 / 失败 9**，全部 `102008 优惠券已被其他订单使用` |
| 5 | **失败的并发请求不写订单** | ✅ 只产生 1 个订单（"锁券在写订单之前"的设计生效） |
| 6 | 下单锁券 + 金额抵扣 | ✅ `100000 → 99000`，券 `0 → 1` |
| 7 | 支付成功核销 | ✅ 订单 `0 → 1`，券 `1 → 2`，流水 `action=3` |
| 8 | **提前返回路径下券能自愈** | ✅ 券 `1 → 2`，且订单 `update_time` 未变 |
| 9 | 错误码跨服务穿透 | ✅ `102003/102004/102008` 均原样到达前端（HTTP 400） |

### commit

`5f7cb86` — feat(coupon): add coupon module (claim / lock / use / release)

`c38cd8a` — fix(order): always sync coupon state, even on the idempotent early-return path

---

## 附：踩坑记录

改造过程中遇到的、值得记录的问题。

### 1. modd 编译失败是静默的

改了代码但容器仍在运行旧二进制，表现为「修复不生效」。

```powershell
docker exec looklook ls -l --time-style=full-iso /go/looklook/data/server/usercenter-api   # 二进制编译时间
docker logs looklook --tail 80 2>&1 | Select-String 'cannot|undefined|error|failed'        # 是否有编译错误
docker restart looklook                                                                    # 强制重新编译
```

**教训**：遇到「修了却没生效」，先确认「当前运行的产物是哪一次构建的」，而不是怀疑修复方案本身。
这与 CI/CD 中「构建失败但仍在跑旧版本」是同一类问题。

> **⭐ 补充（Windows + Docker Desktop 的额外陷阱）**：
> 在 Windows 上，**bind mount 的文件事件通知（inotify）和元数据同步都不可靠**，
> 所以在宿主机上看 `data/server/*` 的 mtime **不能判断容器里的产物是否是最新的**。
> 我本次就踩到了：源码 19:28 改的集成代码**已生效**，19:33 改的另一处**没生效**，
> 而宿主机看到的二进制时间戳全都停留在 19:18 —— **无法据此判断**。
>
> **可靠做法只有两个**：① `docker restart looklook` 强制重新编译；
> ② 看 `docker logs`。**不要拿宿主机上的 mtime 当证据。**
>
> **判断"修复是否真的生效"的正确姿势**：不要看时间戳，**用一个只有新代码才会产生的可观测副作用来判定**
> （本次靠的是"流水表里是否多出一条 `action=3` 记录"）。

### 2. Windows PowerShell 5.1 传含双引号的参数给 git 会被劈开

`git commit -m $msg` 中若消息含 `"`，PowerShell 5.1 不会正确转义，参数被拆成多个，
git 把后半截当成路径：

```
error: pathspec '%+v, req) ...' did not match any file(s) known to git
```

**对策**：多行或含引号的提交信息一律用 `git commit -F <文件>`（把消息写入文件，由 git 读取）。

### 3. `git add` 容易漏掉新增的生成文件

重新生成 pb 代码会新增 `_grpc.pb.go`，提交时容易只 add 修改过的文件。

**对策**：提交前用 `git status --short` 检查有无 `??` 未跟踪文件。

### 4. 删除分支前必须确认已合并

`git branch -d` 允许删除「已合并到其 upstream」的分支（即使尚未合并到 `main`），只给一条 warning：

```
warning: deleting branch '...' that has been merged to 'refs/remotes/origin/...',
         but not yet merged to HEAD
```

**对策**：删除前执行 `git log --oneline main..<branch>`，**输出为空**才说明已完全合并。

### 5. ⭐ `go mod tidy` 会按本地 Go 版本重写 `go.mod`

本地 Go 1.26.5，容器 Go 1.22.1（`GOTOOLCHAIN=local`）。执行 `go mod tidy` 后：

| 改动 | 从 | 到 |
|---|---|---|
| `go` 指令 | `go 1.22` | **`go 1.26.0`** |
| `golang.org/x/crypto` | v0.28.0 | **v0.57.0**（该版本自身要求 `go 1.26.0`） |
| `golang.org/x/net` | v0.30.0 | v0.58.0 |
| `golang.org/x/sys` | v0.26.0 | v0.48.0 |

结果：容器内**所有服务编译失败**（`go.mod requires go >= 1.26.0`），modd 停掉 daemon，
网关返回 **502 Bad Gateway**。

**对策**：
- **依赖已在 `go.mod` 的 require 列表里时，直接 import 即可，无需执行任何命令**
  （`// indirect` 只是注释，`go build` 不校验它）
- 必须 tidy 时先对齐工具链：`GOTOOLCHAIN=go1.22.1 go mod tidy`，或 `go mod tidy -go=1.22`
- 提交前用 `git diff go.mod` 检查 `go` 指令是否被动过

**教训**：**本地工具链版本与容器/CI 不一致时，任何会改写 `go.mod` 的命令都是危险的。**
这类事故在真实工作中同样高频 —— 本地升级了 Go，一次 `go mod tidy` 提交后整条 CI 流水线挂掉。

### 6. `String.Replace` 找不到目标是静默失败的

```powershell
$raw.Replace('old', 'new')   # 找不到就原样返回，不报错、不提示
```

一次用脚本修改 `go.mod` 时，因目标字符串版本号不符（`v0.28.0` vs `v0.57.0`），
替换未生效但命令「看起来成功了」。

**对策**：替换前先判断，并显式打印结果：

```powershell
if ($raw.Contains($old)) { ...; Write-Host 'OK' } else { Write-Host '未找到目标，请检查' }
```

**教训**：**替换操作必须能确认「到底替换成功了没有」** —— 静默失败比报错更危险，
因为你会基于错误的前提继续往下走。（与第 1 条同源：**操作没成功，却没有任何信号**。）

### 7. 只加新路径、不删旧路径 = 重复消息的温床

引入 Outbox 后，业务代码改为「只写 outbox，由 relay 投递」。
但原有的 `pubKqPaySuccess`（直接 `kq.Pusher.Push`）如果只是**不再调用**、**却不删除**，
就会在主干上留下一条"看起来还能用"的旧路径。

**风险**：后续维护者看到它，可能误以为"发消息要调这个"，从而恢复直发 ——
于是同一条业务事件被投递两次（一条来自直发、一条来自 relay），产生**重复消息**。
而重复消息通常**比丢失消息更难排查**：业务状态会被改错，却不一定报错。

**对策**：替换投递路径时，**把旧路径彻底删掉**，而不是留着不调用。
若担心回滚，依赖版本控制即可（需要时从历史提交恢复），而不是把死代码留在主干上。

**这类问题的通用形式**：**"两条都通向目的地的路"比"断路"更危险** ——
断路会报错，双路会静默产生重复。
