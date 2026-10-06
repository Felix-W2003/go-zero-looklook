# 改造记录（REFACTOR）

本仓库 fork 自 [Mikaelemmmm/go-zero-looklook](https://github.com/Mikaelemmmm/go-zero-looklook)（MIT License）。
我在研读其源码的基础上，对其做缺陷修复、分布式一致性加固、安全加固与可观测性改造。

---

## 改造进度

| # | 改造项 | 状态 | 对应 commit |
|---|---|---|---|
| 1 | 修复 gRPC 代码生成漂移导致字段静默丢失 | 🚧 进行中（已定位，修复待提交） | — |
| 2 | 引入事务性发件箱（Outbox）解决支付链路双写不一致 | 📋 计划中 | — |
| 3 | 安全加固：bcrypt 密码 / 日志脱敏 / JWT 缩短有效期 + jti 黑名单 | 📋 计划中 | — |
| 4 | 可观测性：修复失效的日志过滤规则、为 ES 配置 Ingest Pipeline | 📋 计划中 | — |

> **每完成一项，我会补上完整的「问题现象 / 定位过程 / 根因 / 修复 / 验证证据」，并附对应 commit。**
> 未完成的项只保留问题描述，不写未经验证的结论。

---

## 1. 修复 needFood 字段静默丢失

> 状态：🚧 **进行中** —— 问题已定位并确认，修复尚未提交

### 问题现象

订单详情接口返回的 `needFood` 永远为 `0`，但数据库中该订单的 `need_food` 字段实际为 `1`。

### 定位过程

1. 对比「数据库原始值」与「接口返回值」：
   ```sql
   -- 数据库里是对的
   SELECT sn, need_food, order_total_price FROM looklook_order.homestay_order;
   -- HSO2026100217125693042474 | 1 | 112000
   ```
   ```json
   // 接口返回的是 0
   {"orderTotalPrice":1120, "needFood":0, ...}
   ```
2. 逐层向上追溯：api 层 `copier.Copy(&typesOrderDetail, resp.HomestayOrder)` ← rpc 层 `copier.Copy(&resp, homestayOrder)`
3. 比对 `.proto` 与生成的 `.pb.go`：
   - `order.proto` 最大字段号：**27**（`int64 needFood = 27;`）
   - `order.pb.go` 最大字段号：**26** —— 字段 27 不存在

### 根因

`.proto` 被修改后**未重新生成 Go 代码**（代码生成漂移）。

由于 `copier.Copy` 是反射实现、按字段名匹配，**目标结构体没有该字段时会被静默跳过、不报错**，
因此「编译通过、运行不报错、日志干净」，但字段在 rpc → api 的传递途中丢失。

### 修复方案

按 [deploy/script/gencode/gen.sh](deploy/script/gencode/gen.sh) 中的命令重新生成：

```bash
cd app/order/cmd/rpc/pb
goctl rpc protoc order.proto --go_out=../ --go-grpc_out=../ --zrpc_out=../ --style=goZero
```

并编写脚本比对全部 4 个 proto 与对应 `.pb.go` 的最大字段号，确认**仅 order 不同步**（其余 3 个一致）。

### 验证（待执行）

重新生成后重启 order-rpc，确认订单详情接口返回 `needFood=1`，与数据库值一致。

```
$env:MYSQL_PWD = 'PXDN93VRKUm8TeE7'
& 'C:\Program Files\MySQL\MySQL Server 9.2\bin\mysql.exe' -h 127.0.0.1 -P 33069 -u root -t -e `
  "SELECT sn, user_id, need_food, trade_state FROM looklook_order.homestay_order WHERE need_food = 1;"


$gw = 'http://127.0.0.1:8888'

# 2-1) 登录（用户 3 = 13777778888，上面那个订单的归属人）
$login = Invoke-RestMethod -Uri "$gw/usercenter/v1/user/login" -Method Post `
    -ContentType 'application/json' `
    -Body '{"mobile":"13777778888","password":"abc123"}'
$tok = $login.data.accessToken
Write-Host "token 长度: $($tok.Length)"

# 2-2) 查订单详情，重点看 needFood
try {
    $r = Invoke-RestMethod -Uri "$gw/order/v1/homestayOrder/userHomestayOrderDetail" -Method Post `
        -ContentType 'application/json' `
        -Headers @{ Authorization = "Bearer $tok" } `
        -Body '{"sn":"HSO2026100217125693042474"}'
    Write-Host "needFood        = $($r.data.needFood)    <- 期望 1（修复前是 0）"
    Write-Host "orderTotalPrice = $($r.data.orderTotalPrice)"
    Write-Host "tradeState      = $($r.data.tradeState)"
} catch {
    Write-Host "请求失败: $($_.ErrorDetails.Message)"
}
```

### commit

```
+---------------------------+---------+-----------+-------------+
| sn                        | user_id | need_food | trade_state |
+---------------------------+---------+-----------+-------------+
| HSO2026100217125693042474 |       3 |         1 |          -1 |
| HSO2026100419080564475151 |       1 |         1 |          -1 |
+---------------------------+---------+-----------+-------------+

needFood        = 1    <- 期望 1（修复前是 0）
orderTotalPrice = 1120
tradeState      = -1
```
