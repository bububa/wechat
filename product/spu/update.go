package spu

import (
	"github.com/chanxuehong/wechat/product/core"
	"github.com/chanxuehong/wechat/product/model"
)

// Update 更新商品
func Update(clt *core.Client, spu *model.Spu) (product *model.Spu, err error) {
	const incompleteURL = "https://api.weixin.qq.com/product/spu/update?access_token="

	var result struct {
		core.Error
		Data *model.Spu `json:"data"`
	}
	if err = clt.PostJSON(incompleteURL, spu, &result); err != nil {
		return
	}
	if result.ErrCode != core.ErrCodeOK {
		err = &result.Error
		return
	}
	product = result.Data
	return
}
