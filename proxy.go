package go_APIProxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
)

//

//用于存储ip：端口，enable表示当下host是否可用
type OutHost struct {
	DomainName string
	Enable     bool
}

// NewProxyValue 新建一个ProxyValue，但是在调用value之前请自行检查value是否为空
func NewProxyValue(value string) *OutHost {
	ret := &OutHost{
		DomainName: value,
		Enable:     true,
	}
	return ret
}

type Proxy struct {
	//输入的path模板，用于筛选通配符标记的pattern，注意不要直接通过引用修改
	//生产出新的解析模板的函数
	InPathForm func(in string) (map[string]string, error)
	//获得一个经过初始化的路径slice
	OutPathForm func() []string
	MinPathLen  int
	HostsList   []*OutHost
	//初始化时候根据BalancerName来
	BalancerName string
	LoadBalancer LoadBalancer
}

type LoadBalancer func(p *Proxy, r *http.Request) (string, error)
type LoadBalancerFactory func() LoadBalancer

func RoundRobin() LoadBalancer {
	i := 0
	return func(p *Proxy, r *http.Request) (string, error) {
		l := len(p.HostsList)
		if i > l-1 {
			i = 0
		}
		ret := p.HostsList[i].DomainName
		i += 1
		return ret, nil
	}
}

type APIHandler struct {
	Mu      sync.RWMutex
	Proxies map[string]*Proxy
}

func NewAPIHandler() *APIHandler {
	return &APIHandler{
		Proxies: make(map[string]*Proxy),
	}
}

func (h *APIHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := new(httputil.ReverseProxy)
	outHost, outPath, err := CheckAndCreateUrl(h, r)
	if err != nil {
		//这里直接放弃，但是后面会补充一个向本地或远方写错误日志的调用
		return
	}

	p.Director = func(req *http.Request) {
		req.URL.Host = outHost
		req.URL.Scheme = "http"
		req.URL.Path = outPath
	}
	p.ServeHTTP(w, r)
}

//检查输入url是否满足路由规则,并且给出负载均衡后的结论
func CheckAndCreateUrl(handler *APIHandler, r *http.Request) (string, string, error) {
	p, err := handler.MatchRouter(r.URL.Path)
	if err != nil {
		return "", "", err
	}

	outPath, err := CreatPath(p, r.URL.Path)
	if err != nil {
		return "", "", err
	}

	outHost, err := p.LoadBalancer(p, r)
	if err != nil {
		return "", "", err
	}

	return outHost, outPath, nil

}

//根据输入的path参数列表和路由规则，生成出输出的path
func CreatPath(p *Proxy, path string) (string, error) {
	in, err := p.InPathForm(path)
	if err != nil {
		return "", err
	}

	outPathSlice := p.OutPathForm()

	for index, value := range outPathSlice {
		//通配符：
		if value[0] == ':' || value[0] == '*' {
			outPathSlice[index] = in[outPathSlice[index]]
		}

	}
	if outPathSlice[0][0] != '/' {
		outPathSlice[0] = "/" + outPathSlice[0]
	}
	return strings.Join(outPathSlice, "/"), nil

}
func (handler *APIHandler) MatchRouter(path string) (*Proxy, error) {
	for key, proxy := range handler.Proxies {
		if strings.HasPrefix(path, key) {
			return proxy, nil
		}
	}
	err := fmt.Errorf("no router")
	return nil, err

}
func SplitUrl(url string) ([]string, error) {
	l := len(url)
	if l == 0 {
		err := fmt.Errorf("url is empty")
		return []string{}, err
	}
	if url[l-1] == '/' {
		url = url[0 : len(url)-2]
	}
	s := strings.Split(url, "/")
	if s[0] == "" {
		return s[1:], nil
	}
	return s, nil

}

//增加路由，注意处理有/和没有/的问题
func (h *APIHandler) SetKey(inPath string, out string, balancerFactory LoadBalancerFactory) error {
	key := inPath
	for i, v := range inPath {
		if v == ':' || v == '*' {
			key = inPath[0 : i-1]
			break
		}
	}
	inSlice, err := SplitUrl(inPath)
	if err != nil {
		return err
	}

	outSlice, err := SplitUrl(out)
	if err != nil {
		return err
	}

	inParamsFunc := func(path string) (map[string]string, error) {
		ret := make(map[string]string)
		in, err := SplitUrl(path)
		if err != nil {
			return nil, err
		}
		for i, pattern := range inSlice {
			if i > len(in)-1 {
				err = fmt.Errorf("inpath is too short")
				return nil, err
			}
			if pattern[0] == ':' {
				ret[pattern] = in[i]
			}
			if pattern[0] == '*' {
				in[i] = "/" + in[i]
				ret[pattern] = strings.Join(in[i:], "/")
				break
			}
		}
		return ret, nil
	}
	outPathFunc := func() []string {
		return outSlice
	}

	h.Mu.Lock()
	defer h.Mu.Unlock()
	if p, ok := h.Proxies[key]; ok {
		p.InPathForm = inParamsFunc
		p.OutPathForm = outPathFunc
		p.LoadBalancer = balancerFactory()
		return nil
	}
	h.Proxies[key] = &Proxy{
		InPathForm:   inParamsFunc,
		OutPathForm:  outPathFunc,
		LoadBalancer: balancerFactory(),
		HostsList:    make([]*OutHost, 0, 20),
	}
	return nil
}
func (p *APIHandler) DeleteKey(key string) error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if _, ok := p.Proxies[key]; !ok {
		err := fmt.Errorf("key已经存在")
		return err
	}
	delete(p.Proxies, key)
	return nil
}
func (h *APIHandler) AddHost(inPath string, value string) error {
	key := inPath
	for i, v := range inPath {
		if v == ':' || v == '*' {
			key = inPath[0 : i-1]
			break
		}
	}
	if value == "" || inPath == "" {
		err := fmt.Errorf("要添加的key或value为空")
		return err
	}
	h.Mu.Lock()
	defer h.Mu.Unlock()
	if _, ok := h.Proxies[key]; !ok {
		err := fmt.Errorf("要添加的key不存在")
		return err
	}

	h.Proxies[key].HostsList = append(h.Proxies[key].HostsList, NewProxyValue(value))

	return nil
}
