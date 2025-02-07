package chrome

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/sensepost/gowitness/storage"
	"gorm.io/gorm"
)

// Chrome contains information about a Google Chrome
// instance, with methods to run on it.
type Chrome struct {
	ResolutionX int
	ResolutionY int
	UserAgent   string
	Timeout     int64
	Delay       int
	FullPage    bool
	ChromePath  string
	Proxy       string
	Domain      string
        Cookies     []string
}

// NewChrome returns a new initialised Chrome struct
func NewChrome() *Chrome {
	return &Chrome{}
}

// Preflight will preflight a url
func (chrome *Chrome) Preflight(url *url.URL) (resp *http.Response, title string, technologies []string, err error) {
	// purposefully ignore bad certs
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	}

	if chrome.Proxy != "" {
		var erri error
		proxyURL, erri := url.Parse(chrome.Proxy)
		if erri != nil {
			return nil, "", nil, erri
		}
		transport.Proxy = http.ProxyURL(proxyURL)
	}

	// purposefully ignore bad certs
	client := http.Client{
		Transport: transport,
	}

	req, err := http.NewRequest("GET", url.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", chrome.UserAgent)
	req.Close = true

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(chrome.Timeout)*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	resp, err = client.Do(req)
	if err != nil {
		return
	}

	defer resp.Body.Close()
	title, _ = GetHTMLTitle(resp.Body)
	technologies, _ = GetTechnologies(resp)

	return
}

// StorePreflight will store preflight info to a DB
func (chrome *Chrome) StorePreflight(url *url.URL, db *gorm.DB, resp *http.Response, title string, technologies []string, filename string) (uint, error) {

	record := &storage.URL{
		URL:            url.String(),
		FinalURL:       resp.Request.URL.String(),
		ResponseCode:   resp.StatusCode,
		ResponseReason: resp.Status,
		Proto:          resp.Proto,
		ContentLength:  resp.ContentLength,
		Filename:       filename,
		Title:          title,
	}

	// append headers
	for k, v := range resp.Header {
		hv := strings.Join(v, ", ")
		record.AddHeader(k, hv)
	}

	for _, v := range technologies {
		record.AddTechnologie(v)
	}

	// get TLS info, if any
	if resp.TLS != nil {
		record.TLS = storage.TLS{
			Version:    resp.TLS.Version,
			ServerName: resp.TLS.ServerName,
		}

		for _, cert := range resp.TLS.PeerCertificates {
			tlsCert := &storage.TLSCertificate{
				SubjectCommonName:  cert.Subject.CommonName,
				IssuerCommonName:   cert.Issuer.CommonName,
				SignatureAlgorithm: cert.SignatureAlgorithm.String(),
				PubkeyAlgorithm:    cert.PublicKeyAlgorithm.String(),
			}

			for _, name := range cert.DNSNames {
				tlsCert.AddDNSName(name)
			}

			record.TLS.TLSCertificates = append(record.TLS.TLSCertificates, *tlsCert)
		}
	}

	db.Create(record)
	return record.ID, nil
}

// Screenshot takes a screenshot of a URL and saves it to destination
// Ref:
// 	https://github.com/chromedp/examples/blob/255873ca0d76b00e0af8a951a689df3eb4f224c3/screenshot/main.go
func (chrome *Chrome) Screenshot(url *url.URL) ([]byte, error) {

	// setup chromedp default options
	options := []chromedp.ExecAllocatorOption{}
	options = append(options, chromedp.DefaultExecAllocatorOptions[:]...)
	options = append(options, chromedp.UserAgent(chrome.UserAgent))
	options = append(options, chromedp.DisableGPU)
	options = append(options, chromedp.Flag("ignore-certificate-errors", true)) // RIP shittyproxy.go
	options = append(options, chromedp.Flag("blink-settings", "imagesEnabled=false")) // disable gif animation 
	options = append(options, chromedp.WindowSize(chrome.ResolutionX, chrome.ResolutionY))

	if chrome.ChromePath != "" {
		options = append(options, chromedp.ExecPath(chrome.ChromePath))
	}

	if chrome.Proxy != "" {
		options = append(options, chromedp.ProxyServer(chrome.Proxy))
	}

	actx, acancel := chromedp.NewExecAllocator(context.Background(), options...)
	ctx, cancel := chromedp.NewContext(actx)
	defer acancel()
	defer cancel()

	var buf []byte
        if len(chrome.Cookies)>0 {
                  err :=chromedp.Run(ctx, setcookies( chrome.Domain,chrome.Cookies))
                  if err !=err {
					panic(err)
	          }

        }
	// squash JavaScript dialog boxes such as alert();
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if _, ok := ev.(*page.EventJavascriptDialogOpening); ok {
			go func() {
				if err := chromedp.Run(ctx,
					page.HandleJavaScriptDialog(true),
				); err != nil {
					panic(err)
				}
			}()
		}
	})

	if chrome.FullPage {
		// straight from: https://github.com/chromedp/examples/blob/849108f7da9f743bcdaef449699ed57cb4053379/screenshot/main.go

		if err := chromedp.Run(ctx, chromedp.Tasks{
			chromedp.Navigate(url.String()),
			chromedp.Sleep(time.Duration(chrome.Delay) * time.Second),
			chromedp.FullScreenshot(&buf, 100),
		}); err != nil {
			return nil, err
		}

	} else {
		// normal viewport screenshot

		if err := chromedp.Run(ctx, chromedp.Tasks{
			chromedp.Navigate(url.String()),
			chromedp.Sleep(time.Duration(chrome.Delay) * time.Second),
			chromedp.CaptureScreenshot(&buf),
		}); err != nil {
			return nil, err
		}
	}

	return buf, nil
}




// setcookies returns a task to navigate to a host with the passed cookies set
// on the network request.
func setcookies(domain string,cookies []string) chromedp.Tasks {
	if len(cookies)%2 != 0 {
		panic("length of cookies must be divisible by 2")
	}
	return chromedp.Tasks{
		chromedp.ActionFunc(func(ctx context.Context) error {
			// create cookie expiration
			expr := cdp.TimeSinceEpoch(time.Now().Add(180 * 24 * time.Hour))
			// add cookies to chrome
			for i := 0; i < len(cookies); i += 2 {
				err := network.SetCookie(cookies[i], cookies[i+1]).
					WithExpires(&expr).
					WithDomain(domain).
					WithHTTPOnly(true).
					Do(ctx)
				if err != nil {
					return err
				}
			}
			return nil
		}),
         }
}
