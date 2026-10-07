# 优惠券功能 · 设计文档

> **状态**：✅ 设计完成（代码实现见后续提交）
>
> **目标**：从零设计并实现一个优惠券功能，完整走一遍
> `需求 → 领域建模 → 数据库设计 → API 契约 → 代码生成 → 实现 → 验证 → 提交`
>
> **特别说明**：本文档是「设计」，不是「实现记录」。
> 前 4 节完成之前**不写任何业务代码** —— 因为改设计比改代码便宜得多。

---

## 0. 设计决策记录（Decision Log）

> 每做一个设计决策就记一行。**"理由"比"结论"重要** —— 面试官问的从来不是"你用了什么"，而是"为什么"。

### D1｜服务放在哪里？

**结论**：**新建 `app/coupon/`（api + rpc）**，不改动 travel / order 服务。

**理由**：

1. **优惠券是一个独立的业务领域（限界上下文）**，它有自己的生命周期：券模板、发放、领取、锁定、核销、释放、过期回收。这些概念和"民宿"「订单」都没有继承关系。
2. **依赖方向必须是单向的**：
   ```
   coupon-api  ──> coupon-rpc ──> usercenter-rpc   （校验用户，只读）
                                      ▲
   order-rpc ──> coupon-rpc ──────────┘            （下单锁券 / 核销 / 释放）
   ```
   注意 **coupon-rpc 不依赖 order-rpc** —— 订单状态的变化由 order 侧主动通知（`UseCoupon` / `ReleaseCoupon`）。
   如果反过来让 coupon 去查订单，就会形成 **循环依赖**。
3. 相比之下，**挂在 travel 或 order 下的问题**：
   - 一个服务里塞两套不相干的表 → 数据库层面的耦合
   - 改券的活动配置要重新部署民宿服务
   - 未来订单类型扩展（租车/门票）时，券要跟着一起改

### D2｜数据库放哪里？

**结论**：**新建 `looklook_coupon` 库，且只有 coupon 服务能访问**。

**理由**：

1. **Database per Service** 是微服务的硬约束。共用库会退化回"分布式单体"：任何一方改表结构都要双方协调发布。
2. 更根本的问题：**跨服务直接读别人的表是一种隐式耦合**。接口调用有明确的契约（`.proto` / `.api`），改坏了编译期或测试期就能发现；而直接读表没有任何约束，改一个字段名就会在运行时悄悄出问题。
3. 本项目现有的服务都遵守了这个原则（`looklook_usercenter` / `looklook_travel` / `looklook_order` / `looklook_payment` 各自独立）。

### D3｜券的库存如何防超发？

**结论**：用**数据库条件更新（原子 CAS）**：

```sql
UPDATE coupon_template
SET issued_count = issued_count + 1
WHERE id = ? AND issued_count < total_count;
```

**影响行数为 0 → 说明已领完。**

**理由**：

这条 SQL 在数据库层一次性完成了「判断」和「递增」，InnoDB 会对该行加**排他锁**，天然把并发串行化，因此不可能超发。

**对比其他方案：**

| 方案 | 问题 |
|---|---|
| 先 `SELECT` 再 `UPDATE` | **竞态**：两个请求都读到 `issued_count=99 < 100`，都去 `UPDATE` → **超发** |
| Redis 预扣 | 要额外处理「Redis 扣了但 DB 没写」的不一致；一期没必要引入 |
| 分布式锁 | 引入锁超时、续期、误删他人锁等一堆问题；这里用不到 |
| **条件 UPDATE** | ✅ 一条 SQL，数据库保证原子性 |

> **⭐ 这就是"秒杀防超卖"的标准解法** —— 把"检查"和"修改"合并成一条原子语句。

### D4｜同一张券如何防并发使用？

**结论**：用**带状态条件的 UPDATE（CAS）**：

```sql
UPDATE user_coupon
SET status = 1, order_sn = ?, lock_time = NOW()
WHERE id = ? AND user_id = ? AND status = 0;
```

**影响行数为 0 → 说明券已被别人占用（或已使用/已过期）。**

**理由**：

1. 与 D3 同一思路：`WHERE status = 0` 是 CAS 的「比较」，`SET status = 1` 是「交换」。InnoDB 行锁保证**只有一个事务能成功**。
2. **为什么不用 `SELECT ... FOR UPDATE`（悲观锁）？**
   悲观锁必须在事务里一直持有到提交。而锁券之后还要写订单（可能还有别的远程调用），**事务会被拉得很长** —— 这正是我们在 Outbox 改造里讨论过的「**在事务里做网络 I/O**」问题。
   CAS 是一条语句，**锁窗口极短**。
3. **为什么不用分布式锁？** 引入 Redis 依赖 + 锁超时 + 续期 + 误删他人锁等一堆问题。**能用数据库原子性解决的，不要上分布式锁。**

> **⚠️ 一个容易混淆的点**：`FOR UPDATE SKIP LOCKED`（我们在 Outbox relay 扫描任务里用的）是**任务抢占**模式 —— "谁抢到算谁的，抢不到的跳过"。
> 但这里**不能让第二个请求静默"跳过"** —— 用户必须收到「该优惠券已被使用」的**明确错误**。

### D5｜锁券的时机（本题最关键的决策）

**结论**：**下单时锁定**（提交订单 → 锁券 → 写订单）。

**理由（反推法）**：假设设计成「**支付成功后才去核销券**」，看会发生什么：

```
用户 A 有一张券，开两个浏览器窗口，用同一张券下了两笔订单
  → 下单时都不校验券（因为设计成"支付后才核销"）
  → 两笔都付款成功
  → 核销时，两张订单都声称要用这张券
  → 第二张单核销失败
  → ★ 用户付了两笔钱，但只有一笔享受了优惠 → 客诉 / 退款
```

**根本原因**：**券是稀缺资源，谁先占用谁得到。**
所以锁定必须发生在「**确定要用它**」的那一刻（下单），而不是「**钱到账**」的那一刻（支付）。

这也和业务顺序一致：**券在下单时参与算价，价格一旦确定就不能再变。**

### D6｜订单取消 / 超时后，券怎么处理？

**结论**：**释放回「未使用」**（清空 `order_sn`），有效期**不延长**。

**实现方式：复用项目已有的 asynq 延迟队列**（就是「30 分钟自动关单」那套机制）：

- 锁券时**同时排一个延迟任务**：`asynq.ProcessIn(30*time.Minute)`，payload = `{couponId, orderSn}`
- 用户**主动取消**订单 → 立即释放（调 `ReleaseCoupon`）
- **超时未支付** → 延迟任务到期触发，检查订单仍是「待支付」才释放

**两个关键设计点**：

1. **释放前必须校验订单状态** —— 与 `CloseHomestayOrderHandler` 完全一样的幂等思路：只有「待支付」才释放，避免把已支付的订单的券错误释放。
2. **释放也要 CAS**：
   ```sql
   UPDATE user_coupon SET status=0, order_sn='' WHERE order_sn=? AND status=1;
   ```
   影响行数为 0 视为成功（说明券已经不是"锁定"状态了，无需再释放）。

### D7｜券过期如何回收？

**结论**：**定时任务改状态为主 + 查询时兜底过滤**（两者结合）。

**对比：**

| 方案 | 优点 | 缺点 |
|---|---|---|
| **定时任务改状态** | 状态字段永远准确；「我的券」列表查询简单 | 需要调度；任务挂了会积压 |
| 惰性判断（只靠时间比较） | 不需要调度 | 每次查询都要 `AND expire_time > NOW()`；「未使用」里混着已过期的券；统计「有效券数」处处要小心 |
| **两者结合** ⭐ | 定时任务保证最终准确，查询时过滤提供兜底 | 稍微多写一点 |

**一期实现**：复用现有的 `app/mqueue/cmd/scheduler`（已有 asynq cron），定时扫描
`WHERE status=0 AND expire_time < NOW()` 并改为 `status=3`；
同时「我的券列表」在查 `status=0` 时**仍然带上** `AND expire_time > NOW()` 作为兜底。

### D8｜下单时跨服务锁券失败，怎么保证一致？

**结论**：**调整调用顺序 + 明确补偿**，并把「券锁定时长」当作最终一致的兜底。

**调用顺序设计**（每一层都尽量把"有副作用的操作"往后放）：

```
order-rpc 收到下单请求
  ① 参数校验、算价      ── 只读（调 coupon-rpc.PreviewCoupon，可选）
  ② 调 coupon-rpc.LockCoupon   ── 第一个写操作
  ③ 写订单表（状态=待支付）      ── 第二个写操作
  ④ 排延迟任务（关单 + 释放券）  ── 后续动作
```

**各失败点的处理：**

| 失败点 | 后果 | 处理 |
|---|---|---|
| ① 算价失败 | 什么都没写 | 返回错误，**无副作用** ✓ |
| ② 锁券失败 | 订单还没建 | 返回错误，**无副作用** ✓ |
| **② 成功、③ 失败** | ⚠️ **券被锁了但订单没建** | **在 order-rpc 里捕获写订单失败，主动调用 `ReleaseCoupon` 补偿** |
| ③ 成功、④ 失败 | 延迟任务没排 | 兜底：定时任务扫「待支付且已超时」的订单，补做关单+释放 |

**为什么把「锁券」放在「写订单」之前？**
- 反过来（先写订单再锁券）时，锁券失败 → 订单已经建了 → 要么删除订单，要么留一张没券的订单，**处理更脏**
- 先锁券，失败时**订单还不存在** → 状态更干净

**⭐ 为什么这个方案可以接受（关键洞察）：**

> 我们没有引入分布式事务框架（Seata / Saga 框架），而是靠三件事控制不一致窗口：
> 1. **把有副作用的操作尽量往后放**（失败时前面的操作都还没发生）
> 2. **明确的补偿动作**（`ReleaseCoupon`）
> 3. **⭐ 券的锁定本身是有超时的**（30 分钟自动释放）—— **即使补偿失败，系统也会自愈**
>
> 这是「**用最终一致 + 自愈机制替代强一致**」的实用做法。
> 相比之下，引入一个分布式事务框架带来的运维复杂度，在这个业务量级下是不划算的。

### D9｜金额用什么类型存储？

**结论**：**`bigint` 存「分」**。

**理由**：

1. 浮点数**无法精确表示十进制小数**：`0.1 + 0.2 = 0.30000000000000004`。
   金额计算一旦有误差，**对账就会出错**，这是财务场景的大忌。
2. **本项目现有实现用的是 `float64`**（`order.proto` 的 `orderTotalPrice`、`homestay.price` 等）——
   这是一个**已知缺陷**，我们在前面讲 `errors.Wrapf` 时也碰到过金额换算的问题。
3. **新模块不沿用这个坏实践**：所有金额字段一律 `bigint`，单位「分」，字段名带 `_amount` 后缀。

> **⭐ 面试素材**："我在新模块里坚持用整数分存金额，而不是沿用项目里已有的 `float64` —— 因为浮点误差在金额场景不可接受。"

---

## 1. 需求定义

### 1.1 一句话描述

为平台提供优惠券能力：运营配置券模板，用户领券后在下单时抵扣，券有明确的生命周期（未使用 → 锁定 → 核销 / 释放 / 过期）。

### 1.2 用户故事

- 作为**普通用户**，我希望**浏览可领取的优惠券并领取**，以便**下单时省钱**
- 作为**普通用户**，我希望**在「我的券」里按状态查看券**（未使用 / 已使用 / 已过期），以便**知道有哪些券可用**
- 作为**普通用户**，我希望**下单时选择一张券抵扣**，以便**实付更少**
- 作为**普通用户**，我希望**取消订单后券能退回**，以便**不白白损失**
- 作为**平台运营**，我希望**配置券模板（面额、门槛、总量、有效期）**，以便**做营销活动**

### 1.3 功能范围 —— 【做什么】

- [ ] 券模板的配置（**一期用 SQL 种子数据，不做后台管理界面**）
- [ ] 可领券列表（按当前时间过滤有效期内的模板）
- [ ] 用户领券（含库存扣减、每人限领校验）
- [ ] 我的券列表（按状态筛选 + 游标分页）
- [ ] **券预览/算价**（给定订单金额，返回可用券及抵扣后金额）
- [ ] **下单锁定券**
- [ ] **支付成功后核销券**
- [ ] **订单取消 / 超时后释放券**
- [ ] **券过期回收**（定时任务）
- [ ] 核销流水记录（审计）

### 1.4 明确不做 —— 【不做什么】★

- [ ] **不做后台管理界面** —— 一期聚焦用户侧链路；券模板用 SQL 种子数据
- [ ] **不做券的叠加使用** —— 叠加涉及「最优组合计算」，是另一个复杂度量级；一期一笔订单一券
- [ ] **不做券的转赠 / 共享** —— 涉及归属变更和并发，一期无必要
- [ ] **不做券的秒杀抢购** —— 需要 Redis 预扣 + 异步落库；一期用数据库条件更新已足够
- [ ] **不做券的退款处理** —— 订单退款是独立流程，券的退回规则需单独定义
- [ ] **不做多业务类型券** —— 一期只支持民宿订单 `homestayOrder`
- [ ] **不做券的推送 / 到期提醒** —— 属于营销触达，与核心链路无关

### 1.5 验收标准

1. **领券**：超出限领次数时返回明确错误；库存耗尽时返回明确错误
2. **并发领券**：库存 10、并发请求 100 → **恰好发出 10 张**（用并发脚本压测验证）
3. **券列表**：能看到券的状态、面额、门槛、有效期
4. **下单带券**：订单金额 = 原价 − 券抵扣；订单上能查到用了哪张券
5. **并发下单**：同一张券被两个请求同时使用 → **只有一个成功**，另一个返回「优惠券已被使用」
6. **取消订单**：券回到「未使用」，且 `order_sn` 被清空
7. **超时未支付**（30 分钟）：券自动释放
8. **券过期**：定时任务把到期的券改为「已过期」，不再出现在「未使用」分组

### Q1~Q7 答案

| # | 问题 | 答案 |
|---|---|---|
| Q1 | 券有几种类型？ | **一期只做一种：满减券**（满 X 减 Y）。折扣券涉及"折后取整规则"（向上/向下取整），二期再说 |
| Q2 | 券从哪来？ | **用户主动领取**（一期）。后台发放 / 新人自动送需要额外的触发点和权限体系，二期 |
| Q3 | 有效期怎么算？ | **模板固定起止时间**（`valid_start` / `valid_end`）。理由：便于运营控制活动周期，也便于对账；「领取后 N 天」会让同批券的有效期各不相同 |
| Q4 | 每人能领几张？ | **模板级配置 `per_user_limit`**，一期默认 1 |
| Q5 | 一笔订单能用几张？ | **1 张**，不支持叠加 |
| Q6 | 订单取消后券退回吗？ | **退回**，状态回到「未使用」并清空 `order_sn`；**有效期不变**（不延长） |
| Q7 | 有使用门槛吗？ | **有**，`min_amount`（满减门槛），单位分 |

---

## 2. 领域建模

### 2.1 实体

```
CouponTemplate（券模板）—— 运营配置的「券的种类」
  - id
  - name             券名称（如"满 100 减 20"）
  - type             券类型：1 = 满减（一期只有这个）
  - discount_amount  优惠金额（分）
  - min_amount       使用门槛（分）：订单金额需 >= 该值
  - total_count      发行总量
  - issued_count     已发放数量
  - per_user_limit   每人限领数量
  - valid_start      有效期起
  - valid_end        有效期止
  - status           1 = 上架，0 = 下架

UserCoupon（用户券）—— 用户实际持有的一张券
  - id
  - user_id          持有者
  - template_id      来自哪个模板
  - coupon_code      券码（唯一，对外标识）
  - status           0 = 未使用，1 = 已锁定，2 = 已核销，3 = 已过期
  - order_sn         锁定时写入：被哪笔订单占用
  - lock_time        锁定时间
  - use_time         核销时间
  - expire_time      过期时间（领取时从模板复制，便于查询）
  - create_time / update_time

CouponUseRecord（券操作流水）—— 审计用
  - id
  - coupon_code
  - user_id
  - order_sn
  - action           1 = 锁定，2 = 核销，3 = 释放，4 = 过期
  - remark
  - create_time
```

### 2.2 实体关系

```
CouponTemplate   1 ──── N  UserCoupon        一个模板发出多张券
UserCoupon       1 ──── N  CouponUseRecord   一张券有多条操作流水
User（usercenter）1 ─── N  UserCoupon        跨服务：只存 user_id，不建外键
Order（order）      1 ── 0..1 UserCoupon     跨服务：通过 order_sn 关联
```

> **注意**：跨服务的关系**只存 ID、不建外键**。因为外键会跨库（MySQL 不支持跨库外键），
> 而且会引入"服务 A 的写入依赖服务 B 的表存在"这种耦合。

### 2.3 状态机 ★

```
                        ┌──── 订单取消 / 超时释放 ────┐
                        │  CAS: status 1 → 0         │
                        │  清空 order_sn             │
                        ▼                            │
  ┌───────────┐   锁券(下单)   ┌───────────┐  支付成功  ┌───────────┐
  │ 0 未使用  │ ────────────> │ 1 已锁定  │ ────────> │ 2 已核销  │
  └───────────┘   CAS: 0 → 1  └───────────┘  CAS: 1→2  └───────────┘
        │                                                  （终态）
        │ 到期（定时任务）
        │ CAS: 0 → 3
        ▼
  ┌───────────┐
  │ 3 已过期  │  （终态）
  └───────────┘
```

**状态迁移表：**

| 从 | 到 | 触发者 | 条件 | 实现 |
|---|---|---|---|---|
| 0 未使用 | 1 已锁定 | order-rpc（下单） | 券属于该用户、在有效期内、满足门槛 | `UPDATE ... SET status=1, order_sn=? WHERE id=? AND status=0` |
| 1 已锁定 | 2 已核销 | order-mq（支付成功） | 订单支付成功 | `UPDATE ... SET status=2, use_time=NOW() WHERE order_sn=? AND status=1` |
| 1 已锁定 | 0 未使用 | order-rpc（取消）/ asynq 延迟任务（超时） | 订单未支付 | `UPDATE ... SET status=0, order_sn='' WHERE order_sn=? AND status=1` |
| 0 未使用 | 3 已过期 | 定时任务 | `expire_time < NOW()` | `UPDATE ... SET status=3 WHERE status=0 AND expire_time < NOW()` |

### 2.4 业务规则

1. 券只能用于 `service_type = homestayOrder` 的订单（本期唯一业务类型）
2. 门槛校验用的是 **券抵扣前** 的订单金额：`orderAmount >= min_amount`
3. 抵扣后实付金额不能为负：`payAmount = max(orderAmount - discountAmount, 0)`
4. 券不能跨用户使用（`user_id` 必须匹配，**在 WHERE 条件里校验，不只在业务代码里判断**）
5. 券必须在有效期内（`expire_time > NOW()`）
6. 一张券同时只能被一笔订单占用（由状态机 + CAS 保证）
7. **券的过期时间在领取时固定**，不随订单取消而延长
8. 同一张券的**所有状态变更都要写一条 `coupon_use_record`**（审计）

### Q8~Q11 答案

| # | 问题 | 答案 |
|---|---|---|
| Q8 | 有走不到 / 出不去的状态吗？ | **没有。** 0 是入口；2 和 3 是**有意设计的终态**（已核销 / 已过期都不应再变）；1 是**唯一有回退边**的状态 —— 正确，因为只有"占用但未完成"的资源才需要释放 |
| Q9 | 过期怎么处理？ | **定时任务改状态为主 + 查询时兜底过滤**（详见 D7） |
| Q10 | 锁定时要写 `order_sn` 吗？ | **必须写。** ① 释放/核销时靠它定位券（`WHERE order_sn=?`）；② 审计需要知道券被哪笔单占用；③ 幂等的基础（同一订单重复锁券时能识别出"是我自己锁的"） |
| Q11 | 并发下单如何防重复用同一张券？ | **CAS 条件更新**（见 D4）：`WHERE id=? AND status=0`，影响行数 = 0 即竞争失败。**依赖 InnoDB 行锁保证原子性**，不需要分布式锁 |

---

## 3. 数据库设计

### 3.1 表结构

```sql
-- ----------------------------
-- 优惠券模板
-- ----------------------------
DROP TABLE IF EXISTS `coupon_template`;
CREATE TABLE `coupon_template` (
  `id`              bigint       NOT NULL AUTO_INCREMENT,
  `name`            varchar(64)  NOT NULL DEFAULT '' COMMENT '券名称',
  `type`            tinyint      NOT NULL DEFAULT '1' COMMENT '券类型 1满减',
  `discount_amount` bigint       NOT NULL DEFAULT '0' COMMENT '优惠金额(分)',
  `min_amount`      bigint       NOT NULL DEFAULT '0' COMMENT '使用门槛(分)',
  `total_count`     int          NOT NULL DEFAULT '0' COMMENT '发行总量',
  `issued_count`    int          NOT NULL DEFAULT '0' COMMENT '已发放数量',
  `per_user_limit`  int          NOT NULL DEFAULT '1' COMMENT '每人限领数量',
  `valid_start`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '有效期起',
  `valid_end`       datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '有效期止',
  `status`          tinyint      NOT NULL DEFAULT '1' COMMENT '1上架 0下架',
  `create_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_status_valid` (`status`,`valid_end`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='优惠券模板';

-- ----------------------------
-- 用户优惠券
-- ----------------------------
DROP TABLE IF EXISTS `user_coupon`;
CREATE TABLE `user_coupon` (
  `id`          bigint      NOT NULL AUTO_INCREMENT,
  `user_id`     bigint      NOT NULL DEFAULT '0' COMMENT '持有用户id',
  `template_id` bigint      NOT NULL DEFAULT '0' COMMENT '券模板id',
  `coupon_code` varchar(32) NOT NULL DEFAULT '' COMMENT '券码',
  `status`      tinyint     NOT NULL DEFAULT '0' COMMENT '0未使用 1已锁定 2已核销 3已过期',
  `order_sn`    varchar(32) NOT NULL DEFAULT '' COMMENT '占用的订单号',
  `lock_time`   datetime    DEFAULT NULL COMMENT '锁定时间',
  `use_time`    datetime    DEFAULT NULL COMMENT '核销时间',
  `expire_time` datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '过期时间',
  `create_time` datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time` datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_coupon_code` (`coupon_code`),
  KEY `idx_user_status` (`user_id`,`status`),
  KEY `idx_order_sn` (`order_sn`),
  KEY `idx_status_expire` (`status`,`expire_time`),
  KEY `idx_user_template` (`user_id`,`template_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户优惠券';

-- ----------------------------
-- 优惠券操作流水（审计）
-- ----------------------------
DROP TABLE IF EXISTS `coupon_use_record`;
CREATE TABLE `coupon_use_record` (
  `id`          bigint       NOT NULL AUTO_INCREMENT,
  `coupon_code` varchar(32)  NOT NULL DEFAULT '' COMMENT '券码',
  `user_id`     bigint       NOT NULL DEFAULT '0',
  `order_sn`    varchar(32)  NOT NULL DEFAULT '',
  `action`      tinyint      NOT NULL DEFAULT '0' COMMENT '1锁定 2核销 3释放 4过期',
  `remark`      varchar(255) NOT NULL DEFAULT '',
  `create_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_coupon_code` (`coupon_code`),
  KEY `idx_order_sn` (`order_sn`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='优惠券操作流水';
```

### 3.2 关键查询与索引

| # | 查询场景 | SQL 大致形态 | 命中索引 |
|---|---|---|---|
| 1 | 我的券列表（按状态） | `WHERE user_id=? AND status=? AND expire_time>NOW() ORDER BY id DESC LIMIT ?` | `idx_user_status` |
| 2 | 领券时校验限领 | `SELECT COUNT(*) WHERE user_id=? AND template_id=?` | `idx_user_template` |
| 3 | 库存扣减 | `UPDATE ... SET issued_count=issued_count+1 WHERE id=? AND issued_count<total_count` | 主键 |
| 4 | 锁券 | `UPDATE ... SET status=1,order_sn=? WHERE id=? AND status=0` | 主键 |
| 5 | 按订单核销 / 释放 | `WHERE order_sn=? AND status=1` | `idx_order_sn` |
| 6 | 过期回收扫描 | `WHERE status=0 AND expire_time<NOW()` | `idx_status_expire` |
| 7 | 可领券列表 | `WHERE status=1 AND valid_start<=NOW() AND valid_end>=NOW()` | `idx_status_valid` |

> **索引设计原则**：每个索引都对应上表里的一个**具体查询**，不是"看着可能有用就加"。
> 索引会拖慢写入并占用空间，**没有查询支撑的索引就是负资产**。

### Q12~Q16 答案

| # | 问题 | 答案 |
|---|---|---|
| Q12 | 金额用什么类型？ | **`bigint` 存「分」**（详见 D9）。**不沿用项目现有的 `float64`** |
| Q13 | 券码怎么生成？用唯一索引吗？ | 用**随机串**（`crypto/rand` 生成 16 字节 → hex，共 32 字符）。**必须建唯一索引**：① 防止极小概率的碰撞；② 券码是对外唯一标识，**靠数据库约束比靠代码校验可靠** |
| Q14 | 需要乐观锁 `version` 字段吗？ | **不需要。** 因为所有状态迁移都用"带状态条件的 UPDATE"（CAS），`status` 字段**本身就充当了版本号**，再加 `version` 是重复的。（对比：`third_payment` 用了 `version`，因为它做的是**整行覆盖更新**，没有状态字段可以借力） |
| Q15 | 发行量放模板表还是单独表？ | **放模板表。** ① 一张模板只有一个库存计数器，1:1 拆表没有收益；② `UPDATE ... WHERE issued_count < total_count` 需要**在同一行**才能原子判断 |
| Q16 | 需要软删除吗？ | **模板表需要**（下架用 `status=0` 而不是删除，因为已发出的券还引用它）。**用户券不需要** —— 终态（已核销/已过期）本身就是一种软删 |

---

## 4. API 契约设计

### 4.1 api 层（对外 HTTP 接口）

| # | 方法 | 路径 | 用途 | 需要 JWT | 请求字段 | 响应字段 |
|---|---|---|---|---|---|---|
| 1 | POST | `/coupon/v1/coupon/templateList` | 可领券列表 | **否** | `pageSize`, `lastId` | `list[{templateId, name, discountAmount, minAmount, validStart, validEnd, remainCount}]` |
| 2 | POST | `/coupon/v1/coupon/claim` | 领取券 | 是 | `templateId` | `couponCode` |
| 3 | POST | `/coupon/v1/coupon/myCoupons` | 我的券（按状态） | 是 | `status`, `pageSize`, `lastId` | `list[{couponCode, name, discountAmount, minAmount, status, expireTime}]` |
| 4 | POST | `/coupon/v1/coupon/previewCoupon` | 下单前算价 | 是 | `amount`（订单原价，分） | `list[{couponCode, discountAmount, finalAmount}]` |

**路由分组**：① 不挂 JWT；②③④ 挂 `rest.WithJwt(...)` —— 与项目现有写法一致。

### 4.2 rpc 层（内部服务间调用）

| # | 方法 | 用途 | **调用方** | 是否幂等 |
|---|---|---|---|---|
| 1 | `ClaimCoupon(userId, templateId)` | 领券 | coupon-api | ✅ 靠「每人限领」校验天然幂等 |
| 2 | `ListUserCoupons(userId, status)` | 我的券 | coupon-api | ✅ 只读 |
| 3 | `PreviewCoupon(userId, amount)` | 算可用券 | order-rpc | ✅ 只读 |
| 4 | **`LockCoupon(userId, couponCode, orderSn)`** | **下单锁券** | **order-rpc** | ⚠️ **必须幂等**（见下） |
| 5 | `UseCoupon(orderSn)` | 支付后核销 | order-mq | ⚠️ 必须幂等 |
| 6 | `ReleaseCoupon(orderSn)` | 取消 / 超时释放 | order-rpc、asynq 任务 | ⚠️ 必须幂等 |

### ⭐ 幂等的统一实现模式

**核心原则：影响行数为 0 时不要直接报错，而是回查当前状态，判断是否符合预期。**

```go
// LockCoupon 的幂等实现示意
res := UPDATE user_coupon SET status=1, order_sn=? WHERE id=? AND status=0
if res.RowsAffected == 1 {
    return OK                                    // 首次锁定成功
}
// 影响 0 行 → 回查，判断是不是"我自己已经锁过了"
row := SELECT status, order_sn FROM user_coupon WHERE id=?
switch {
case row.status == 1 && row.order_sn == orderSn:
    return OK                                    // ★ 重复调用（order-rpc 超时重试）→ 幂等返回成功
case row.status == 0:
    return ErrLockFailed                         // 并发竞争失败（极少见）
case row.status == 1:
    return ErrCouponUsedByOtherOrder             // 被别的订单占用
default:
    return ErrCouponStatusInvalid                // 已核销/已过期
}
```

**`UseCoupon` / `ReleaseCoupon` 同理**：`WHERE order_sn=? AND status=<期望值>`，
影响 0 行时先判断「是不是已经是目标状态」，是则返回成功。

> **⭐ 为什么幂等在这里是"必须项"而不是"加分项"**：
> - `order-rpc` 调 `coupon-rpc` 时若网络超时，**调用方无法判断到底成功了没有**
> - 如果重试会造成重复副作用 → 调用方只能选择"不重试"（可能漏）或"重试"（可能重）
> - **有了幂等，调用方才可以放心重试** —— 这是分布式系统里重试机制的前提

### 4.3 错误码

**⚠️ 重要前提**：本项目有两套错误返回路径（[pkg/result/httpResult.go](pkg/result/httpResult.go)）：

```go
if e, ok := causeErr.(*xerr.CodeError); ok {
    // 本地错误：直接用自定义码和消息
} else if gstatus, ok := status.FromError(causeErr); ok {
    if xerr.IsCodeErr(grpcCode) {      // ★ 跨服务错误要走白名单！
        errcode = grpcCode; errmsg = gstatus.Message()
    }
}
```

**所以新增业务错误码时，必须同时改两个文件**：

1. [pkg/xerr/errCode.go](pkg/xerr/errCode.go) —— 定义常量
2. [pkg/xerr/errMsg.go](pkg/xerr/errMsg.go) —— **加进 `message` 白名单**（否则 rpc 返回的错误到 api 层会被替换成「服务器开小差」）

**按项目约定（前 3 位业务 + 后 3 位功能），优惠券模块用 `102xxx`：**

| 错误码 | 常量名 | 含义 | 提示语 |
|---|---|---|---|
| 102001 | `COUPON_TEMPLATE_NOT_FOUND` | 券模板不存在 | 优惠券不存在 |
| 102002 | `COUPON_SOLD_OUT` | 券已领完 | 优惠券已领完 |
| 102003 | `COUPON_EXCEED_USER_LIMIT` | 超出每人限领 | 您已达到领取上限 |
| 102004 | `COUPON_NOT_FOUND` | 用户券不存在 | 优惠券不存在 |
| 102005 | `COUPON_ALREADY_USED` | 券已被使用 | 优惠券已被使用 |
| 102006 | `COUPON_EXPIRED` | 券已过期 | 优惠券已过期 |
| 102007 | `COUPON_BELOW_MIN_AMOUNT` | 不满足使用门槛 | 订单金额未满门槛，无法使用 |
| 102008 | `COUPON_OCCUPIED_BY_OTHER_ORDER` | 券被其他订单占用 | 优惠券已被其他订单使用 |
| 102009 | `COUPON_STATUS_INVALID` | 券状态不允许该操作 | 优惠券状态异常 |

### Q17~Q20 答案

| # | 问题 | 答案 |
|---|---|---|
| Q17 | 哪些接口需要 JWT？ | ① `templateList` **不需要**（未登录也应能浏览活动，起转化作用）；②③④ 需要 |
| Q18 | `LockCoupon` 幂等吗？ | **必须幂等**（实现见上方「幂等的统一实现模式」）。**关键是不能只判断"影响行数=0 就报错"**，要区分「被我自己的订单锁的」和「被别人锁的」 |
| Q19 | 分页怎么设计？ | 沿用项目现有的 **`lastId + pageSize`（游标分页）**。理由：① 与现有接口一致（`userHomestayOrderList` 就是这个形式）；② `WHERE id < lastId` 走主键索引，**深分页不退化**（对比 `OFFSET` 在深分页时越来越慢） |
| Q20 | 跨服务调用要设超时吗？ | **必须设。** `zrpc` 默认超时 2000ms。**超时的处理是关键**：不能当成功、也不能当失败 —— 因为**锁券可能已经成功了**。所以 `LockCoupon` 超时后应由**幂等设计**兜底：重试一次，靠 `LockCoupon` 的幂等返回正确结果，而不是直接报错 |

---

## 5. 已知风险与后续演进

| # | 风险 / 局限 | 一期处理 | 二期演进方向 |
|---|---|---|---|
| 1 | `LockCoupon` 成功但写订单失败 | `order-rpc` 捕获异常后调 `ReleaseCoupon` 补偿；**并用券的 30 分钟自动释放兜底** | 引入 Saga 编排，或把"锁券"纳入订单的本地事务（Outbox 思路的变体） |
| 2 | 补偿动作本身失败 | 券会在 30 分钟后自动释放（自愈） | 加补偿失败告警 + 定时对账任务 |
| 3 | 券过期靠定时扫描，可能有延迟 | 查询时**兜底过滤** `expire_time > NOW()` | 领取时排 asynq 延迟任务，到期精确触发 |
| 4 | 仅支持一种券类型 | 满减券 | 折扣券、兑换券（需要抽象"优惠计算"策略） |
| 5 | 不支持叠加 | 一笔订单一券 | 需要"最优组合"算法 |
| 6 | 金额单位是分，但项目其他模块是 `float64` | coupon 模块内部统一用分 | 推动全项目金额类型统一（属于跨模块重构） |

---

## 6. 实现计划（下一步）

设计评审通过后，按以下顺序实现：

```
⑤ 代码生成
   - goctl api go   → app/coupon/cmd/api 骨架
   - goctl rpc protoc → app/coupon/cmd/rpc 骨架
⑥ 基础设施
   - 建库 looklook_coupon + 三张表 + 券模板种子数据
   - pkg/xerr 新增 102xxx 错误码（errCode.go + errMsg.go 两处）
   - modd.conf 增加 coupon-api / coupon-rpc
   - nginx 增加 /coupon/ 路由
   - 两个 etc/*.yaml 配置
⑦ 业务实现（按依赖顺序）
   - model 层：三个表的数据访问
   - coupon-rpc：ClaimCoupon（含库存 CAS）→ ListUserCoupons → PreviewCoupon
                 → LockCoupon（含幂等）→ UseCoupon → ReleaseCoupon
   - coupon-api：四个接口的 logic
   - 定时任务：券过期回收（复用 app/mqueue/cmd/scheduler）
   - 跨服务集成：order-rpc 下单时调 LockCoupon；order-mq 支付成功后调 UseCoupon
⑧ 测试验证
   - 手工链路：领券 → 我的券 → 下单抵扣 → 支付核销 / 取消释放 → 超时释放 → 过期回收
   - 并发测试：库存 10 + 并发 100 领券 → 恰好 10 张（验收标准 2）
   - 并发测试：同一张券并发下单 → 只成功一个（验收标准 5）
   - 幂等测试：重复调 LockCoupon / UseCoupon / ReleaseCoupon
⑨ 文档
   - 本文档定稿（补充实现与验证证据）
   - REFACTOR.md 新增一项
⑩ 提交 / PR
```

---

## 7. 实现阶段新增的设计决策

> 设计文档不是写完就冻结的。编码过程中遇到的新问题、以及做出的新判断，同样要记录下来。

### D10｜`user_coupon` 为什么要快照 `discount_amount` / `min_amount`？

**结论**：**领取时从模板复制到用户券上，而不是每次 JOIN 模板表去读。**

**理由**：

1. **正确性（最重要）**：如果实时读模板，**运营改了模板的优惠金额后，已发出的券会跟着缩水** ——
   用户领的是"满 100 减 20"，模板改成"减 10"后他的券就贬值了。这是数据事故。
2. **性能**：「预览可用券」是高频查询，快照后**不需要 JOIN**，一条单表查询即可。
3. **与项目一致**：`order` 表同样存了 `title` / `price` 的快照。

> **⭐ 反过来：`name`（券名称）故意不快照** —— 它只是展示信息。
> **判断标准：这个字段会不会影响金额计算 / 权益判定？会 → 快照；不会 → 实时取。**

### D11｜model 层为什么**不使用缓存**？

**结论**：`goctl model mysql datasource` 生成时**不带 `-c`**。

**理由**：

1. 券的状态是**强一致要求**：读到脏的 `status=0` 会导致"预览列出了已被锁定的券"。
2. **更根本的原因**：goctl 生成的缓存 model **只会对自己的 `Update` 方法做缓存失效**。
   而我们的状态迁移全是**自定义 CAS SQL**（`UPDATE ... WHERE status=0`），
   **不会触发缓存失效** → 缓存会一直脏到 TTL 过期。
3. 核心读写都绕过了缓存 —— 那缓存只带来风险，不带来收益。

> **该用缓存的数据**：读多写少、容忍短暂不一致（民宿详情、商品信息）。
> **不该用的**：状态频繁迁移、且读到的值直接决定后续写操作（本项目）。

### D12｜限领校验的并发缺陷（TOCTOU）与修复

**问题**：最初把限领校验放在**事务外**，并发下会超领：

```
请求 A: count=0 → 通过          ┐
请求 B: count=0 → 通过          ├ 两个请求都看到 0
A: BEGIN → IncrIssuedCount     │  然后各自插一张券
B: BEGIN → IncrIssuedCount     │
A/B: INSERT user_coupon        ┘  → 同一用户领到 2 张
```

**修复（三个要点，缺一不可）**：

| # | 做法 | 为什么 |
|---|---|---|
| 1 | 把校验**移进事务** | 与发券操作原子 |
| 2 | 放在**扣库存之后** | `IncrIssuedCount` 会锁住模板行，**把同一模板的并发请求串行化** |
| 3 | 用 `SELECT ... FOR UPDATE`（**当前读**） | RR 隔离级别下普通 SELECT 是**快照读**，看不到并发事务刚提交的插入 |

**校验失败直接 `return error` → 事务回滚 → 刚扣的库存自动退回，无需补偿代码。**

> **代价**：`FOR UPDATE` 在 RR 下会加 gap lock，高并发有死锁风险。
> 一期可接受；QPS 上来后应改为 Redis 原子计数，或在入口做幂等键。

### D13｜`LockCoupon` 为什么要带 `amount`？

**结论**：**门槛校验由券服务自己保证，不信任调用方。**

**理由**：`PreviewCoupon` 只是"预览"，客户端完全可以不调它直接调 `LockCoupon`。
若门槛校验只放在算价阶段，等于**把风控交给了调用方** —— 恶意调用方能用满减券去买 1 分钱的订单。

**设计原则**：**服务自己负责守住自己的业务规则**；调用方的校验只是"提前反馈"，不能作为依赖。

### D14｜幂等的统一实现模式

`LockCoupon` / `UseCoupon` / `ReleaseCoupon` 共用同一套写法：

```
1. CAS 更新（WHERE 带状态条件）
2. 影响行数 == 1  → 成功，写流水，返回
3. 影响行数 == 0  → **不直接报错**，回查当前状态：
     · 已是「目标状态」 → 重复调用 → 幂等返回成功
     · 是别的状态       → 真正的错误 → 返回明确错误码
```

**`LockCoupon` 为什么要在 CAS 之前额外判断一次？**
因为它要区分「被我自己的订单锁的」（→ 成功）和「被别人锁的」（→ `COUPON_OCCUPIED_BY_OTHER_ORDER`）。

**调用方（order-rpc）为什么需要这个？**
跨服务调用超时时，调用方**无法判断到底成功了没有**。
只有被调方幂等，调用方才敢重试；否则只能"不重试"（可能漏）或"重试"（可能重）。

### D15｜为什么加了 `id` / `templateId` 之外的字段到 `pb.UserCoupon`

「我的券」列表要展示券名称和面额，因此 `pb.UserCoupon` 增加了
`discountAmount` / `minAmount`（来自快照）与 `name`（实时取自模板，同一模板只查一次避免 N+1）。
