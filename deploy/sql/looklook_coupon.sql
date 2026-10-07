/*
 Navicat MySQL Data Transfer

 Source Server         : looklook
 Source Server Type    : MySQL
 Source Server Version : 80028
 Source Host           : 127.0.0.1:33069
 Source Schema         : looklook_coupon

 Target Server Type    : MySQL
 Target Server Version : 80028
 File Encoding         : 65001

 Date: 2026-10-07
*/

SET NAMES utf8mb4;
SET FOREIGN_KEY_CHECKS = 0;

-- ----------------------------
-- 建库
-- ----------------------------
CREATE DATABASE IF NOT EXISTS `looklook_coupon` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
USE `looklook_coupon`;

-- ----------------------------
-- 优惠券模板
-- 说明：券的「种类」，运营配置。issued_count 与 total_count 在同一行，
--       才能用一条 UPDATE 原子地完成「判断是否领完 + 递增」。
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
-- 说明1：status 字段本身充当乐观锁版本号 —— 所有状态迁移都用
--        「带 status 条件的 UPDATE」(CAS)，因此不需要额外的 version 列。
-- 说明2：discount_amount / min_amount 是【领取时的快照】。
--        必须在领取时从模板复制过来，而不是每次 JOIN 模板表去读：
--        因为运营改了模板的优惠金额后，已发出的券不应该跟着变
--        （与 order 表存 title/price 快照是同一个道理）。
-- ----------------------------
DROP TABLE IF EXISTS `user_coupon`;
CREATE TABLE `user_coupon` (
  `id`              bigint      NOT NULL AUTO_INCREMENT,
  `user_id`         bigint      NOT NULL DEFAULT '0' COMMENT '持有用户id',
  `template_id`     bigint      NOT NULL DEFAULT '0' COMMENT '券模板id',
  `discount_amount` bigint      NOT NULL DEFAULT '0' COMMENT '优惠金额(分,领取时快照)',
  `min_amount`      bigint      NOT NULL DEFAULT '0' COMMENT '使用门槛(分,领取时快照)',
  `coupon_code`     varchar(32) NOT NULL DEFAULT '' COMMENT '券码',
  `status`          tinyint     NOT NULL DEFAULT '0' COMMENT '0未使用 1已锁定 2已核销 3已过期',
  `order_sn`        varchar(32) NOT NULL DEFAULT '' COMMENT '占用的订单号',
  `lock_time`       datetime    DEFAULT NULL COMMENT '锁定时间',
  `use_time`        datetime    DEFAULT NULL COMMENT '核销时间',
  `expire_time`     datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '过期时间',
  `create_time`     datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time`     datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_coupon_code` (`coupon_code`),
  KEY `idx_user_status` (`user_id`,`status`),
  KEY `idx_order_sn` (`order_sn`),
  KEY `idx_status_expire` (`status`,`expire_time`),
  KEY `idx_user_template` (`user_id`,`template_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户优惠券';

-- ----------------------------
-- 优惠券操作流水（审计）
-- ⚠️ action 的取值必须与 app/coupon/model/status.go 的常量保持一致：
--      1 = 领取(CouponActionClaim)   2 = 锁定(CouponActionLock)
--      3 = 核销(CouponActionUse)     4 = 释放(CouponActionRelease)
--      5 = 过期(CouponActionExpire)
-- ----------------------------
DROP TABLE IF EXISTS `coupon_use_record`;
CREATE TABLE `coupon_use_record` (
  `id`          bigint       NOT NULL AUTO_INCREMENT,
  `coupon_code` varchar(32)  NOT NULL DEFAULT '' COMMENT '券码',
  `user_id`     bigint       NOT NULL DEFAULT '0' COMMENT '用户id',
  `order_sn`    varchar(32)  NOT NULL DEFAULT '' COMMENT '订单号',
  `action`      tinyint      NOT NULL DEFAULT '0' COMMENT '1领取 2锁定 3核销 4释放 5过期',
  `remark`      varchar(255) NOT NULL DEFAULT '' COMMENT '备注',
  `create_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  KEY `idx_coupon_code` (`coupon_code`),
  KEY `idx_order_sn` (`order_sn`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='优惠券操作流水';

-- ----------------------------
-- 种子数据：券模板
-- 一期不做后台管理界面，券模板用 SQL 直接配置
-- ----------------------------
INSERT INTO `coupon_template` (`name`, `type`, `discount_amount`, `min_amount`, `total_count`, `issued_count`, `per_user_limit`, `valid_start`, `valid_end`, `status`) VALUES
('满100减20', 1, 2000, 10000, 100, 0, 1, '2026-01-01 00:00:00', '2027-12-31 23:59:59', 1),
('满500减80', 1, 8000, 50000, 50,  0, 1, '2026-01-01 00:00:00', '2027-12-31 23:59:59', 1),
('新人立减10元', 1, 1000, 0,   200, 0, 1, '2026-01-01 00:00:00', '2027-12-31 23:59:59', 1);

SET FOREIGN_KEY_CHECKS = 1;
