package dingtalk

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/kubesphere/notification-manager/pkg/async"
	"github.com/kubesphere/notification-manager/pkg/notify/config"
	"github.com/kubesphere/notification-manager/pkg/notify/notifier"
	"github.com/kubesphere/notification-manager/pkg/utils"
	"github.com/prometheus/alertmanager/template"
)

const (
	URL                          = "https://oapi.dingtalk.com/"
	DefaultSendTimeout           = time.Second * 3
	DefaultTemplate              = `{{ template "nm.default.text" . }}`
	MarkdownTemplate             = `{{ template "nm.default.markdown" . }}`
	ConversationMessageMaxSize   = 5000
	ChatbotMessageMaxSize        = 19960
	DefaultExpires               = time.Hour * 2
	DefaultChatbotThreshold      = 20
	DefaultChatbotUnit           = time.Minute
	DefaultChatbotWaitTime       = time.Second * 10
	DefaultConversationThreshold = 25
	DefaultConversationUnit      = time.Second
)

type Notifier struct {
	notifierCfg                *config.Config
	DingTalk                   []*config.DingTalk
	timeout                    time.Duration
	logger                     log.Logger
	template                   *notifier.Template
	templateName               string
	tmplType                   string
	throttle                   *Throttle
	ats                        *notifier.AccessTokenService
	tokenExpires               time.Duration
	conversationMessageMaxSize int
	chatbotMessageMaxSize      int
	chatbotThreshold           int
	chatbotUnit                time.Duration
	chatbotMaxWaitTime         time.Duration
	conversationThreshold      int
	conversationUnit           time.Duration
	conversationMaxWaitTime    time.Duration
}

type dingtalkMessageContent struct {
	Content string `json:"content"`
}

type at struct {
	AtMobiles []string `yaml:"atMobiles,omitempty" json:"atMobiles,omitempty"`
}

type dingtalkMessage struct {
	Text dingtalkMessageContent `yaml:"text,omitempty" json:"text,omitempty"`
	ID   string                 `yaml:"chatid,omitempty" json:"chatid,omitempty"`
	Type string                 `yaml:"msgtype,omitempty" json:"msgtype,omitempty"`
	At   at                     `yaml:"at,omitempty" json:"at,omitempty"`
}

type dingtalkMarkdown struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type dingtalkMarkdownMessage struct {
	Markdown dingtalkMarkdown `yaml:"markdown,omitempty" json:"markdown,omitempty"`
	ID       string           `yaml:"chatid,omitempty" json:"chatid,omitempty"`
	Type     string           `yaml:"msgtype,omitempty" json:"msgtype,omitempty"`
	At       at               `yaml:"at,omitempty" json:"at,omitempty"`
}

type response struct {
	Code    int    `json:"errcode"`
	Message string `json:"errmsg"`
	Token   string `json:"access_token"`
	Status  int    `json:"status"`
	Punish  string `json:"punish"`
}

func NewDingTalkNotifier(logger log.Logger, receivers []config.Receiver, notifierCfg *config.Config) notifier.Notifier {

	var path []string
	opts := notifierCfg.ReceiverOpts
	if opts != nil && opts.Global != nil {
		path = opts.Global.TemplateFiles
	}
	tmpl, err := notifier.NewTemplate(path)
	if err != nil {
		_ = level.Error(logger).Log("msg", "DingTalkNotifier: get template error", "error", err.Error())
		return nil
	}

	n := &Notifier{
		notifierCfg:                notifierCfg,
		timeout:                    DefaultSendTimeout,
		logger:                     logger,
		template:                   tmpl,
		templateName:               DefaultTemplate,
		tmplType:                   "text",
		throttle:                   GetThrottle(),
		ats:                        notifier.GetAccessTokenService(),
		tokenExpires:               DefaultExpires,
		conversationMessageMaxSize: ConversationMessageMaxSize,
		chatbotMessageMaxSize:      ChatbotMessageMaxSize,
		chatbotThreshold:           DefaultChatbotThreshold,
		chatbotUnit:                DefaultChatbotUnit,
		chatbotMaxWaitTime:         DefaultChatbotWaitTime,
		conversationThreshold:      DefaultConversationThreshold,
		conversationUnit:           DefaultConversationUnit,
		conversationMaxWaitTime:    DefaultConversationUnit,
	}

	if opts != nil && opts.DingTalk != nil {

		d := opts.DingTalk

		if d.NotificationTimeout != nil {
			n.timeout = time.Second * time.Duration(*d.NotificationTimeout)
		}

		if len(d.Template) > 0 {
			n.templateName = d.Template
		} else if opts.Global != nil && len(opts.Global.Template) > 0 {
			n.templateName = opts.Global.Template
		}

		if len(d.TmplType) > 0 && strings.Compare("markdown", d.TmplType) == 0 {
			n.tmplType = d.TmplType
			n.templateName = MarkdownTemplate
		}

		if d.TokenExpires != 0 {
			n.tokenExpires = d.TokenExpires
		}

		if d.ConversationMessageMaxSize > 0 {
			n.conversationMessageMaxSize = d.ConversationMessageMaxSize
		}

		if d.ChatbotMessageMaxSize > 0 {
			n.chatbotMessageMaxSize = d.ChatbotMessageMaxSize
		}

		if d.ChatBotThrottle != nil {
			t := d.ChatBotThrottle
			if t.Threshold > 0 {
				n.chatbotThreshold = t.Threshold
			}

			if t.Unit != 0 {
				n.chatbotUnit = t.Unit
			}

			if t.MaxWaitTime != 0 {
				n.chatbotMaxWaitTime = t.MaxWaitTime
			}
		}

		if d.ConversationThrottle != nil {
			t := d.ConversationThrottle
			if t.Threshold > 0 {
				n.conversationThreshold = t.Threshold
			}

			if t.Unit != 0 {
				n.conversationUnit = t.Unit
			}

			if t.MaxWaitTime != 0 {
				n.conversationMaxWaitTime = t.MaxWaitTime
			}
		}
	}

	for _, r := range receivers {
		receiver, ok := r.(*config.DingTalk)
		if !ok || receiver == nil {
			continue
		}

		//if receiver.DingTalkConfig == nil {
		//	_ = level.Warn(logger).Log("msg", "DingTalkNotifier: ignore receiver because of empty config")
		//	continue
		//}

		n.DingTalk = append(n.DingTalk, receiver)
	}

	return n
}

func (n *Notifier) Notify(ctx context.Context, data template.Data) []error {

	group := async.NewGroup(ctx)
	for _, dingtalk := range n.DingTalk {
		d := dingtalk

		newData := utils.FilterAlerts(data, d.Selector, n.logger)
		if len(newData.Alerts) == 0 {
			continue
		}

		if d.ChatBot != nil {
			group.Add(func(stopCh chan interface{}) {
				stopCh <- n.sendToChatBot(ctx, d, utils.FilterAlerts(newData, d.Selector, n.logger))
			})
		}

		if d.ChatIDs != nil && len(d.ChatIDs) > 0 {
			group.Add(func(stopCh chan interface{}) {
				stopCh <- n.sendToConversation(ctx, d, utils.FilterAlerts(newData, d.Selector, n.logger))
			})
		}
	}

	return group.Wait()
}

func (n *Notifier) sendToChatBot(ctx context.Context, d *config.DingTalk, data template.Data) []error {

	bot := d.ChatBot

	webhook, err := n.notifierCfg.GetCredential(bot.Webhook)
	if err != nil {
		_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: get webhook secret error", "error", err.Error())
		return []error{err}
	}
	var templateName string
	send := func(msg string) error {
		// get the phones, start
		atPhones := []string{}
		for k, v := range msg {
			if strings.Compare(string(v), "@") == 0 {
				atPhones = append(atPhones, msg[k+1:k+12])
			}
		}
		// end
		start := time.Now()
		defer func() {
			_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: send message to chatbot", "used", time.Since(start).String())
		}()

		var buf bytes.Buffer
		if strings.Compare("markdown", d.TmplType) == 0 || (len(d.TmplType) == 0 && strings.Compare("markdown", n.tmplType) == 0) {
			templateName = MarkdownTemplate
			dm := dingtalkMarkdownMessage{
				Type: "markdown",
				Markdown: dingtalkMarkdown{
					Title: "Alert Notice",
					Text:  msg,
				},
				At: at{
					AtMobiles: atPhones,
				},
			}
			if err := utils.JsonEncode(&buf, dm); err != nil {
				_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: encode markdown message error", "error", err.Error())
				return err
			}
		} else {
			templateName = DefaultTemplate
			dm := dingtalkMessage{
				Type: "text",
				Text: dingtalkMessageContent{
					Content: msg,
				},
				At: at{
					AtMobiles: atPhones,
				},
			}
			if err := utils.JsonEncode(&buf, dm); err != nil {
				_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: encode text message error", "error", err.Error())
				return err
			}
		}

		secret := ""
		if bot.Secret != nil {
			secret, err = n.notifierCfg.GetCredential(bot.Secret)
			if err != nil {
				_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: get chatbot secret error", "error", err.Error())
				return err
			}
		}

		u := webhook
		if len(secret) > 0 {
			timestamp, sign := calcSign(secret)
			p := make(map[string]string)
			p["timestamp"] = timestamp
			p["sign"] = sign
			u, err = utils.UrlWithParameters(webhook, p)
			if err != nil {
				_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: set parameters error", "error", err)
				return err
			}
		}

		request, err := http.NewRequest(http.MethodPost, u, &buf)
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: create http request error", "error", err)
			return err
		}
		request.Header.Set("Content-Type", "application/json")

		body, err := utils.DoHttpRequest(context.Background(), nil, request)
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: do http error", "error", err)
			return err
		}

		res := &response{}
		if err := utils.JsonUnmarshal(body, res); err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: decode response body error", "error", err)
			return err
		}

		if res.Code != 0 {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: send message to chatbot error", "errcode", res.Code, "errmsg", res.Message)
			return err
		}

		if res.Status != 0 {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: send message to chatbot error", "name", "status", res.Status, "punish", res.Punish)
			return err
		}

		_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: send message to chatbot")

		return nil
	}

	keywords := ""
	if bot.Keywords != nil && len(bot.Keywords) > 0 {
		keywords = "\n\n[Keywords] "
		for _, k := range bot.Keywords {
			keywords = fmt.Sprintf("%s%s, ", keywords, k)
		}

		keywords = strings.TrimSuffix(keywords, ", ")
	}

	messages, err := n.template.Split(data, n.chatbotMessageMaxSize-len(keywords), templateName, n.logger)
	if err != nil {
		_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: split message error", "error", err.Error())
		return []error{err}
	}

	group := async.NewGroup(ctx)
	for _, m := range messages {
		msg := fmt.Sprintf("%s%s", m, keywords)
		group.Add(func(stopCh chan interface{}) {
			n.throttle.TryAdd(webhook, n.chatbotThreshold, n.chatbotUnit, n.chatbotMaxWaitTime)
			if n.throttle.Allow(webhook, n.logger) {
				stopCh <- send(msg)
			} else {
				_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: message to chatbot dropped because of flow control")
				stopCh <- fmt.Errorf("")
			}
		})
	}

	return group.Wait()
}

func (n *Notifier) sendToConversation(ctx context.Context, d *config.DingTalk, data template.Data) []error {

	appkey, err := n.notifierCfg.GetCredential(d.DingTalkConfig.AppKey)
	if err != nil {
		_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: get appkey error", "error", err)
		return []error{err}
	}

	appsecret, err := n.notifierCfg.GetCredential(d.DingTalkConfig.AppSecret)
	if err != nil {
		_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: get appsecret error", "error", err)
		return []error{err}
	}
	var templateName string
	send := func(chatID, msg string) error {

		start := time.Now()
		defer func() {
			_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: send message to conversation", "conversation", chatID, "used", time.Since(start).String())
		}()

		token, err := n.getToken(ctx, appkey, appsecret)
		if err != nil {
			_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: get token error", "conversation", chatID, "error", err)
			return err
		}
		var buf bytes.Buffer
		if strings.Compare("markdown", d.TmplType) == 0 || (len(d.TmplType) == 0 && strings.Compare("markdown", n.tmplType) == 0) {
			templateName = MarkdownTemplate
			dm := dingtalkMarkdownMessage{
				Type: "markdown",
				Markdown: dingtalkMarkdown{
					Title: "Alert Notice",
					Text:  msg,
				},
				ID: chatID,
			}
			if err := utils.JsonEncode(&buf, dm); err != nil {
				_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: encode markdown message error", "conversation", chatID, "error", err.Error())
				return err
			}
		} else {
			templateName = DefaultTemplate
			dm := dingtalkMessage{
				Text: dingtalkMessageContent{
					Content: msg,
				},
				Type: "text",
				ID:   chatID,
			}
			if err := utils.JsonEncode(&buf, dm); err != nil {
				_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: encode text message error", "conversation", chatID, "error", err.Error())
				return err
			}
		}

		u, err := utils.UrlWithPath(URL, "chat/send")
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: set path error", "conversation", chatID, "error", err)
			return err
		}

		p := make(map[string]string)
		p["access_token"] = token
		u, err = utils.UrlWithParameters(u, p)
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: set parameters error", "conversation", chatID, "error", err)
			return err
		}

		request, err := http.NewRequest(http.MethodPost, u, &buf)
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: create http request error", "conversation", chatID, "error", err)
			return err
		}
		request.Header.Set("Content-Type", "application/json")

		body, err := utils.DoHttpRequest(context.Background(), nil, request)
		if err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: do http error", "conversation", chatID, "error", err)
			return err
		}

		res := &response{}
		if err := utils.JsonUnmarshal(body, res); err != nil {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: decode response body error", "conversation", chatID, "error", err)
			return err
		}

		if res.Code != 0 {
			_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: send message to conversation error", "conversation", chatID, "errcode", res.Code, "errmsg", res.Message)
			return err
		}

		_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: send message to conversation", "conversation", chatID)

		return nil
	}

	messages, err := n.template.Split(data, n.conversationMessageMaxSize, templateName, n.logger)
	if err != nil {
		_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: split message error", "error", err.Error())
		return nil
	}

	group := async.NewGroup(ctx)
	for _, m := range messages {
		msg := m
		for _, chatID := range d.ChatIDs {
			id := chatID
			group.Add(func(stopCh chan interface{}) {
				n.throttle.TryAdd(appkey, n.conversationThreshold, n.conversationUnit, n.conversationMaxWaitTime)
				if n.throttle.Allow(appkey, n.logger) {
					stopCh <- send(id, msg)
				} else {
					_ = level.Error(n.logger).Log("msg", "DingTalkNotifier: message to conversation dropped because of flow control", "conversation", chatID)
					stopCh <- fmt.Errorf("")
				}
			})
		}
	}

	return group.Wait()
}

func (n *Notifier) getToken(ctx context.Context, appkey, appsecret string) (string, error) {

	get := func(ctx context.Context) (string, time.Duration, error) {
		u, err := utils.UrlWithPath(URL, "gettoken")
		if err != nil {
			return "", 0, err
		}

		p := make(map[string]string)
		p["appkey"] = appkey
		p["appsecret"] = appsecret

		u, err = utils.UrlWithParameters(u, p)
		if err != nil {
			return "", 0, err
		}

		request, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return "", 0, err
		}
		request.Header.Set("Content-Type", "application/json")

		body, err := utils.DoHttpRequest(context.Background(), nil, request)
		if err != nil {
			return "", 0, err
		}

		res := &response{}
		if err := utils.JsonUnmarshal(body, res); err != nil {
			return "", 0, err
		}

		if res.Code != 0 {
			return "", 0, fmt.Errorf("errcode %d, errmsg %s", res.Code, res.Message)
		}

		_ = level.Debug(n.logger).Log("msg", "DingTalkNotifier: get token", "key", appkey+" | "+appsecret)
		return res.Token, n.tokenExpires, nil
	}

	return n.ats.GetToken(ctx, appkey+" | "+appsecret, get)
}

func calcSign(secret string) (string, string) {

	timestamp := fmt.Sprintf("%d", time.Now().Unix()*1000)
	msg := fmt.Sprintf("%s\n%s", timestamp, secret)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(msg))
	sign := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return timestamp, url.QueryEscape(sign)
}
