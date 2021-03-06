package wxacode

import (
	"encoding/json"
	"github.com/chanxuehong/wechat/mp/core"
)

func Create(clt *core.Client, request Request) (data []byte, err error) {
	const incompleteURL = "https://api.weixin.qq.com/cgi-bin/wxaapp/createwxaqrcode?access_token="
	data, err = PostJSON(clt, incompleteURL, &request)
	if err != nil {
		return
	}
	var result core.Error
	json.Unmarshal(data, &result)
	if result.ErrCode != core.ErrCodeOK {
		err = &result
		return
	}
	return
}
