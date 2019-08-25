package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/chanxuehong/wechat/internal/debug/api"
	"github.com/chanxuehong/wechat/util"
)

type Ticket struct {
	AppId                 string `xml:"AppId" json:"AppId"`                                 // 第三方平台appid
	ComponentVerifyTicket string `xml:"ComponentVerifyTicket" json:"ComponentVerifyTicket"` // Ticket内容
}

type TicketStorage interface {
	Get() (ticket *Ticket, err error)
	Put(ticket *Ticket) error
}

type TokenUpdateHandler func(token string, err error)

// access_token 中控服务器接口.
type AccessTokenServer interface {
	UpdateTicket(ticket *Ticket) error
	Token() (token string, err error)                           // 请求中控服务器返回缓存的 access_token
	RefreshToken(currentToken string) (token string, err error) // 请求中控服务器刷新 access_token
}

var _ AccessTokenServer = (*DefaultAccessTokenServer)(nil)

// DefaultAccessTokenServer 实现了 AccessTokenServer 接口.
//  NOTE:
//  1. 用于单进程环境.
//  2. 因为 DefaultAccessTokenServer 同时也是一个简单的中控服务器, 而不是仅仅实现 AccessTokenServer 接口,
//     所以整个系统只能存在一个 DefaultAccessTokenServer 实例!
type DefaultAccessTokenServer struct {
	appId         string
	appSecret     string
	ticketStorage TicketStorage
	httpClient    *http.Client

	refreshTokenRequestChan  chan string             // chan currentToken
	refreshTokenResponseChan chan refreshTokenResult // chan {token, err}

	tokenCache unsafe.Pointer // *accessToken

	updateTokenCallback TokenUpdateHandler
}

// NewDefaultAccessTokenServer 创建一个新的 DefaultAccessTokenServer, 如果 httpClient == nil 则默认使用 util.DefaultHttpClient.
func NewDefaultAccessTokenServer(appId, appSecret string, ticketStorage TicketStorage, httpClient *http.Client) (srv *DefaultAccessTokenServer) {
	if httpClient == nil {
		httpClient = util.DefaultHttpClient
	}

	srv = &DefaultAccessTokenServer{
		appId:                    url.QueryEscape(appId),
		appSecret:                url.QueryEscape(appSecret),
		httpClient:               httpClient,
		ticketStorage:            ticketStorage,
		refreshTokenRequestChan:  make(chan string),
		refreshTokenResponseChan: make(chan refreshTokenResult),
	}

	go srv.tokenUpdateDaemon(time.Hour*1 + time.Minute*50)
	return
}

func (srv *DefaultAccessTokenServer) SetUpdateTokenCallback(h TokenUpdateHandler) {
	srv.updateTokenCallback = h
}

func (srv *DefaultAccessTokenServer) Token() (token string, err error) {
	if p := (*accessToken)(atomic.LoadPointer(&srv.tokenCache)); p != nil {
		return p.Token, nil
	}
	return srv.RefreshToken("")
}

func (srv *DefaultAccessTokenServer) UpdateTicket(ticket *Ticket) error {
	return srv.ticketStorage.Put(ticket)
}

type refreshTokenResult struct {
	token string
	err   error
}

func (srv *DefaultAccessTokenServer) RefreshToken(currentToken string) (token string, err error) {
	srv.refreshTokenRequestChan <- currentToken
	rslt := <-srv.refreshTokenResponseChan
	return rslt.token, rslt.err
}

func (srv *DefaultAccessTokenServer) tokenUpdateDaemon(initTickDuration time.Duration) {
	tickDuration := initTickDuration

NEW_TICK_DURATION:
	ticker := time.NewTicker(tickDuration)
	for {
		select {
		case currentToken := <-srv.refreshTokenRequestChan:
			accessToken, cached, err := srv.updateToken(currentToken)
			if err != nil {
				srv.refreshTokenResponseChan <- refreshTokenResult{err: err}
				break
			}
			srv.refreshTokenResponseChan <- refreshTokenResult{token: accessToken.Token}
			if !cached {
				tickDuration = time.Duration(accessToken.ExpiresIn) * time.Second
				ticker.Stop()
				goto NEW_TICK_DURATION
			}

		case <-ticker.C:
			accessToken, _, err := srv.updateToken("")
			if err != nil {
				break
			}
			newTickDuration := time.Duration(accessToken.ExpiresIn) * time.Second
			if abs(tickDuration-newTickDuration) > time.Second*5 {
				tickDuration = newTickDuration
				ticker.Stop()
				goto NEW_TICK_DURATION
			}
		}
	}
}

func abs(x time.Duration) time.Duration {
	if x >= 0 {
		return x
	}
	return -x
}

type accessToken struct {
	Token     string `json:"component_access_token"`
	ExpiresIn int64  `json:"expires_in"`
}

// updateToken 从微信服务器获取新的 access_token 并存入缓存, 同时返回该 access_token.
func (srv *DefaultAccessTokenServer) updateToken(currentToken string) (token *accessToken, cached bool, err error) {
	if currentToken != "" {
		if p := (*accessToken)(atomic.LoadPointer(&srv.tokenCache)); p != nil && currentToken != p.Token {
			return p, true, nil // 无需更改 p.ExpiresIn 参数值, cached == true 时用不到
		}
	}
	ticket, err := srv.ticketStorage.Get()
	if err != nil {
		atomic.StorePointer(&srv.tokenCache, nil)
		if srv.updateTokenCallback != nil {
			srv.updateTokenCallback("", err)
		}
		return
	}
	tokenRequest := map[string]string{
		"component_appid":         srv.appId,
		"component_appsecret":     srv.appSecret,
		"component_verify_ticket": ticket.ComponentVerifyTicket,
	}
	payload, err := json.Marshal(tokenRequest)
	if err != nil {
		atomic.StorePointer(&srv.tokenCache, nil)
		if srv.updateTokenCallback != nil {
			srv.updateTokenCallback("", err)
		}
		return
	}
	httpResp, err := srv.httpClient.Post("https://api.weixin.qq.com/cgi-bin/component/api_component_token", "application/json", bytes.NewReader(payload))
	if err != nil {
		atomic.StorePointer(&srv.tokenCache, nil)
		if srv.updateTokenCallback != nil {
			srv.updateTokenCallback("", err)
		}
		return
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode != http.StatusOK {
		atomic.StorePointer(&srv.tokenCache, nil)
		err = fmt.Errorf("http.Status: %s", httpResp.Status)
		return
	}

	var result struct {
		Error
		accessToken
	}
	if err = api.DecodeJSONHttpResponse(httpResp.Body, &result); err != nil {
		atomic.StorePointer(&srv.tokenCache, nil)
		return
	}
	if result.ErrCode != ErrCodeOK {
		atomic.StorePointer(&srv.tokenCache, nil)
		err = &result.Error
		if srv.updateTokenCallback != nil {
			srv.updateTokenCallback("", err)
		}
		return
	}

	// 由于网络的延时, access_token 过期时间留有一个缓冲区
	switch {
	case result.ExpiresIn > 31556952: // 60*60*24*365.2425
		atomic.StorePointer(&srv.tokenCache, nil)
		err = errors.New("expires_in too large: " + strconv.FormatInt(result.ExpiresIn, 10))
		return
	case result.ExpiresIn > 60*60:
		result.ExpiresIn -= 60 * 10
	case result.ExpiresIn > 60*30:
		result.ExpiresIn -= 60 * 5
	case result.ExpiresIn > 60*5:
		result.ExpiresIn -= 60
	case result.ExpiresIn > 60:
		result.ExpiresIn -= 10
	default:
		atomic.StorePointer(&srv.tokenCache, nil)
		err = errors.New("expires_in too small: " + strconv.FormatInt(result.ExpiresIn, 10))
		return
	}

	tokenCopy := result.accessToken
	atomic.StorePointer(&srv.tokenCache, unsafe.Pointer(&tokenCopy))
	token = &tokenCopy
	if srv.updateTokenCallback != nil {
		srv.updateTokenCallback(token, nil)
	}
	return
}
