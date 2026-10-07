package logic

import (
	"context"

	"looklook/app/usercenter/cmd/rpc/internal/svc"
	"looklook/app/usercenter/cmd/rpc/usercenter"
	"looklook/app/usercenter/model"
	"looklook/pkg/tool"
	"looklook/pkg/xerr"

	"github.com/pkg/errors"
	"github.com/zeromicro/go-zero/core/logx"
)

type LoginLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

var ErrGenerateTokenError = xerr.NewErrMsg("生成token失败")
var ErrUsernamePwdError = xerr.NewErrMsg("账号或密码不正确")

func NewLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogic {
	return &LoginLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *LoginLogic) Login(in *usercenter.LoginReq) (*usercenter.LoginResp, error) {

	var userId int64
	var err error
	switch in.AuthType {
	case model.UserAuthTypeSystem:
		userId, err = l.loginByMobile(in.AuthKey, in.Password)
	default:
		return nil, xerr.NewErrCode(xerr.SERVER_COMMON_ERROR)
	}
	if err != nil {
		return nil, err
	}

	//2、Generate the token, so that the service doesn't call rpc internally
	generateTokenLogic := NewGenerateTokenLogic(l.ctx, l.svcCtx)
	tokenResp, err := generateTokenLogic.GenerateToken(&usercenter.GenerateTokenReq{
		UserId: userId,
	})
	if err != nil {
		return nil, errors.Wrapf(ErrGenerateTokenError, "GenerateToken userId : %d", userId)
	}

	return &usercenter.LoginResp{
		AccessToken:  tokenResp.AccessToken,
		AccessExpire: tokenResp.AccessExpire,
		RefreshAfter: tokenResp.RefreshAfter,
	}, nil
}

func (l *LoginLogic) loginByMobile(mobile, password string) (int64, error) {

	user, err := l.svcCtx.UserModel.FindOneByMobile(l.ctx, mobile)
	if err != nil && err != model.ErrNotFound {
		return 0, errors.Wrapf(xerr.NewErrCode(xerr.DB_ERROR), "根据手机号查询用户信息失败，mobile:%s,err:%v", mobile, err)
	}
	if user == nil {
		return 0, errors.Wrapf(ErrUserNoExistsError, "mobile:%s", mobile)
	}

	if !l.verifyAndMigratePassword(user, password) {
		return 0, errors.Wrap(ErrUsernamePwdError, "密码匹配出错")
	}

	return user.Id, nil
}

func (l *LoginLogic) loginBySmallWx() error {
	return nil
}

// verifyAndMigratePassword 校验密码，并对存量 MD5 哈希做渐进式迁移（Lazy Migration）。
//
// 迁移策略：
//
//   - 存储值已是 bcrypt：直接用 bcrypt 校验；
//
//   - 存储值还是 MD5：先用 MD5 校验，通过后立即用 bcrypt 重新加密并更新数据库，
//     该用户下次登录即走 bcrypt 分支，实现无感迁移。
//
//     安全约束：本函数内不得记录 password 的明文或任何哈希值（详见 REFACTOR 第 2 项）。
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
		// 升级失败不影响本次登录（用户密码本身是正确的），只记日志，下次登录会重试
		l.Errorf("migrate password to bcrypt failed, userId=%d, err=%v", user.Id, err)
		return true
	}

	user.Password = hashed
	if _, err := l.svcCtx.UserModel.Update(l.ctx, nil, user); err != nil {
		// 更新失败同样不影响本次登录，下次登录会重试
		l.Errorf("update bcrypt password failed, userId=%d, err=%v", user.Id, err)
	}
	return true
}
