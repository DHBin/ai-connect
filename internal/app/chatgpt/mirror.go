package chatgpt

import (
	"context"
	"encoding/json"
	"github.com/dhbin/ai-connect/internal/common"
	"github.com/dhbin/ai-connect/internal/common/code"
	"github.com/dhbin/ai-connect/internal/common/web"
	"github.com/dhbin/ai-connect/internal/config"
	"github.com/dhbin/ai-connect/templates"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/spf13/cobra"
	"golang.org/x/net/proxy"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var proxyClient = http.Client{}

func RunMirror() {
	mirrorConfig := config.ChatGptMirror()
	tls := mirrorConfig.Tls

	// 从环境变量获取代理地址
	proxyEnv := os.Getenv("PROXY_SERVER")
	if proxyEnv != "" {
		proxyURL, err := url.Parse(proxyEnv)
		if err != nil {
			cobra.CheckErr(err)
		}
		if proxyURL.Scheme == "socks5" || proxyURL.Scheme == "socks5h" {
			var auth *proxy.Auth
			if proxyURL.User != nil {
				username := proxyURL.User.Username()
				password, _ := proxyURL.User.Password()
				auth = &proxy.Auth{
					User:     username,
					Password: password,
				}
			}
			dialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, proxy.Direct)
			if err != nil {
				cobra.CheckErr(err)
			}
			transport := &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				},
			}
			proxyClient.Transport = transport
		} else {
			// 处理 HTTP(s) 代理
			proxyClient.Transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			}
		}
	} else {
		// 如果没有设置代理，使用默认传输
		proxyClient.Transport = http.DefaultTransport
	}

	e := echo.New()

	// 创建并加载模板
	tmpl := &templates.Template{
		Templates: template.Must(template.ParseFS(templates.TemplateFs, "chatgpt/*.html")),
	}
	e.Renderer = tmpl

	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(RebuildRequestURL)

	e.GET("/", handleIndex)
	e.GET("/chatgpt/hook.js", returnHookJs)
	e.GET("/c/*", handleIndex)
	e.POST("/backend-api/accounts/logout_all", func(c echo.Context) error {
		return c.JSON(http.StatusForbidden, nil)
	})
	e.GET("/gpts", handleGpts)
	e.Any("/webrtc/*", web.ProxyWebSocket(buildTargetUrl))
	e.Any("/*", handle)

	if tls.Enabled {
		err := e.StartTLS(mirrorConfig.Address, tls.Cert, tls.Key)
		cobra.CheckErr(err)
	} else {
		err := e.Start(mirrorConfig.Address)
		cobra.CheckErr(err)

	}
}

func handleIndex(c echo.Context) error {
	token := "announce"
	t := c.QueryParam("token")
	if t != "" {
		token = t
		c.SetCookie(&http.Cookie{
			Name:    "token",
			Value:   token,
			Expires: time.Now().Add(24 * time.Hour),
		})
	}
	data := map[string]string{
		"StaticPrefixUrl": c.Scheme() + "://" + c.Request().Host,
		"Token":           token,
	}
	return c.Render(http.StatusOK, "index.html", data)
}

func returnHookJs(c echo.Context) error {
	bs, err := templates.TemplateFs.ReadFile("chatgpt/hook.js")
	if err != nil {
		return err
	}
	return c.Blob(http.StatusOK, "application/javascript", bs)
}

func handleGpts(c echo.Context) error {
	if c.QueryParam("_data") == "routes/gpts._index" {
		return c.JSON(http.StatusOK, checkGpts{
			Kind:     "store",
			Referrer: "https://chatgpt.com/",
		})
	}
	return handleGIndex(c)
}

func handleGIndex(c echo.Context) error {
	token := "announce"
	t := c.QueryParam("token")
	if t != "" {
		token = t
		c.SetCookie(&http.Cookie{
			Name:    "token",
			Value:   token,
			Secure:  true,
			Expires: time.Now().Add(24 * time.Hour),
		})
	}
	data := map[string]string{
		"StaticPrefixUrl": c.Scheme() + "://" + c.Request().Host,
		"Token":           token,
	}
	return c.Render(http.StatusOK, "gpts.html", data)
}

func handle(c echo.Context) error {

	u := c.Request().URL
	sourceHost := u.Host

	if strings.HasSuffix(u.Path, ".map") {
		return c.NoContent(http.StatusNotFound)
	}

	if strings.HasPrefix(u.Path, "/g/") && c.QueryParam("_data") == "" {
		return handleGIndex(c)
	}

	// 构建目标url
	targetUrl := buildTargetUrl(u)

	// 构建目标headers
	targetHeaders := make(http.Header)
	for k, v := range c.Request().Header {
		if filterHeader(k) {
			continue
		}
		newV := strings.ReplaceAll(strings.Join(v, ","), sourceHost, targetUrl.Host)
		targetHeaders.Add(k, newV)
	}

	targetHeaders.Set("Referer", targetUrl.String())
	targetHeaders.Set("Origin", targetUrl.Scheme+"://"+targetUrl.Host)

	reqBs, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}
	// 重置body
	reqBody := strings.ReplaceAll(string(reqBs), sourceHost, targetUrl.Host)
	reqReader := strings.NewReader(reqBody)

	// 转发请求到目标url
	req, err := http.NewRequest(c.Request().Method, targetUrl.String(), reqReader)
	if err != nil {
		return err
	}
	req.Header = targetHeaders
	if needAuth(u.Path) {
		token, err := c.Cookie("token")

		if err == nil && token.Value != "" {
			req.Header.Set("Authorization", "Bearer "+dealToken(token.Value))
		}
	}

	resp, err := proxyClient.Do(req)
	if err != nil {
		return err
	}

	contentEncoding := resp.Header.Get("Content-Encoding")

	setIfNotEmpty(c.Response().Header(), resp.Header, "Content-Encoding")
	setIfNotEmpty(c.Response().Header(), resp.Header, "Content-Type")
	setIfNotEmpty(c.Response().Header(), resp.Header, "Cache-Control")
	setIfNotEmpty(c.Response().Header(), resp.Header, "Expires")

	if strings.HasPrefix(u.Path, "/g/") && c.QueryParam("_data") == "routes/g.$gizmoId._index" {
		reader, err := code.WarpReader(resp.Body, contentEncoding)
		if err != nil {
			return err
		}
		defer common.IgnoreErr(reader.Close)

		targetBs, err := io.ReadAll(reader)
		if err != nil {
			return err
		}
		r := make(map[string]interface{})
		err = json.Unmarshal(targetBs, &r)
		if err != nil {
			return err
		}

		for k, v := range gptsInfoInject {
			r[k] = v
		}

		writer, err := code.WarpWriter(c.Response(), contentEncoding)
		if err != nil {
			return err
		}
		defer common.IgnoreErr(writer.Close)
		newR, err := json.Marshal(r)
		if err != nil {
			return err
		}
		_, err = writer.Write(newR)

		return err
	}

	if u.Path == "/backend-api/conversation" {
		reader, err := code.WarpReader(resp.Body, contentEncoding)
		if err != nil {
			return err
		}
		defer common.IgnoreErr(reader.Close)
		writer, err := code.WarpWriter(c.Response(), contentEncoding)
		if err != nil {
			return err
		}
		defer common.IgnoreErr(writer.Close)
		bs := make([]byte, 1)
		for {
			n, err := reader.Read(bs)
			if err != nil && err != io.EOF {
				return err
			}
			if n == 0 {
				break
			}
			_, _ = writer.Write(bs)
			c.Response().Flush()
		}
		return nil
	}

	// 设置响应状态码
	c.Response().WriteHeader(resp.StatusCode)

	if bodyNeedHandle(u) && resp.StatusCode < http.StatusMultipleChoices {
		reader, err := code.WarpReader(resp.Body, contentEncoding)
		if err != nil {
			return err
		}
		defer common.IgnoreErr(reader.Close)
		bs, err := io.ReadAll(reader)
		if err != nil {
			return err
		}

		body := string(bs)

		if u.Path == "/backend-api/me" {
			var meJson me
			err := json.Unmarshal(bs, &meJson)
			if err == nil {
				meJson.Email = "sam@openai.com"
				meJson.PhoneNumber = nil
				meJson.Name = "Sam Altman"
				for i := range meJson.Orgs.Data {
					meJson.Orgs.Data[i].Description = "Personal org for " + meJson.Email
				}

				newMe, err := json.Marshal(meJson)
				if err == nil {
					body = string(newMe)
				}
			}
		} else {
			sourceScheme := u.Scheme
			body = strings.ReplaceAll(body, "https://chatgpt.com", sourceScheme+"://"+sourceHost)
			body = strings.ReplaceAll(body, "https://ab.chatgpt.com", sourceScheme+"://"+sourceHost+"/ab")
			body = strings.ReplaceAll(body, "https://cdn.oaistatic.com", sourceScheme+"://"+sourceHost)
			body = strings.ReplaceAll(body, "chatgpt.com", sourceHost)
		}
		writer, err := code.WarpWriter(c.Response(), contentEncoding)
		if err != nil {
			return err
		}
		defer common.IgnoreErr(writer.Close)
		_, err = writer.Write([]byte(body))
		return err
	}

	_, _ = io.Copy(c.Response(), resp.Body)

	return nil
}
