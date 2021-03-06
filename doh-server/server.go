/*
    DNS-over-HTTPS
    Copyright (C) 2017 Star Brilliant <m13253@hotmail.com>

    Permission is hereby granted, free of charge, to any person obtaining a
    copy of this software and associated documentation files (the "Software"),
    to deal in the Software without restriction, including without limitation
    the rights to use, copy, modify, merge, publish, distribute, sublicense,
    and/or sell copies of the Software, and to permit persons to whom the
    Software is furnished to do so, subject to the following conditions:

    The above copyright notice and this permission notice shall be included in
    all copies or substantial portions of the Software.

    THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
    IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
    FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
    AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
    LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
    FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER
    DEALINGS IN THE SOFTWARE.
*/

package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"golang.org/x/net/idna"
	"github.com/gorilla/handlers"
	"github.com/miekg/dns"
	"../json-dns"
)

type Server struct {
	conf		*config
	udpClient	*dns.Client
	tcpClient	*dns.Client
	servemux	*http.ServeMux
}

func NewServer(conf *config) (s *Server) {
	s = &Server {
		conf: conf,
		udpClient: &dns.Client {
			Net: "udp",
			Timeout: time.Duration(conf.Timeout) * time.Second,
		},
		tcpClient: &dns.Client {
			Net: "tcp",
			Timeout: time.Duration(conf.Timeout) * time.Second,
		},
		servemux: http.NewServeMux(),
	}
	s.servemux.HandleFunc(conf.Path, s.handlerFunc)
	return
}

func (s *Server) Start() error {
	servemux := http.Handler(s.servemux)
	if s.conf.Verbose {
		servemux = handlers.CombinedLoggingHandler(os.Stdout, servemux)
	}
	if s.conf.Cert != "" || s.conf.Key != "" {
		return http.ListenAndServeTLS(s.conf.Listen, s.conf.Cert, s.conf.Key, servemux)
	} else {
		return http.ListenAndServe(s.conf.Listen, servemux)
	}
}

func (s *Server) handlerFunc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.Header().Set("Server", "DNS-over-HTTPS/1.0 (+https://github.com/m13253/dns-over-https)")
	w.Header().Set("X-Powered-By", "DNS-over-HTTPS/1.0 (+https://github.com/m13253/dns-over-https)")

	name := r.FormValue("name")
	if name == "" {
		jsonDNS.FormatError(w, "Invalid argument value: \"name\"", 400)
		return
	}
	name = strings.ToLower(name)
	if punycode, err := idna.ToASCII(name); err == nil {
		name = punycode
	} else {
		jsonDNS.FormatError(w, fmt.Sprintf("Invalid argument value: \"name\" = %q (%s)", name, err.Error()), 400)
		return
	}

	rrTypeStr := r.FormValue("type")
	rrType := uint16(1)
	if rrTypeStr == "" {
	} else if v, err := strconv.ParseUint(rrTypeStr, 10, 16); err == nil {
		rrType = uint16(v)
	} else if v, ok := dns.StringToType[strings.ToUpper(rrTypeStr)]; ok {
		rrType = v
	} else {
		jsonDNS.FormatError(w, fmt.Sprintf("Invalid argument value: \"type\" = %q", rrTypeStr), 400)
		return
	}

	cdStr := r.FormValue("cd")
	cd := false
	if cdStr == "1" || cdStr == "true" {
		cd = true
	} else if cdStr == "0" || cdStr == "false" || cdStr == "" {
	} else {
		jsonDNS.FormatError(w, fmt.Sprintf("Invalid argument value: \"cd\" = %q", cdStr), 400)
		return
	}

	ednsClientSubnet := r.FormValue("edns_client_subnet")
	ednsClientFamily := uint16(0)
	ednsClientAddress := net.IP(nil)
	ednsClientNetmask := uint8(255)
	if ednsClientSubnet != "" {
		if ednsClientSubnet == "0/0" {
			ednsClientSubnet = "0.0.0.0/0"
		}
		slash := strings.IndexByte(ednsClientSubnet, '/')
		if slash < 0 {
			ednsClientAddress = net.ParseIP(ednsClientSubnet)
			if ednsClientAddress == nil {
				jsonDNS.FormatError(w, fmt.Sprintf("Invalid argument value: \"edns_client_subnet\" = %q", ednsClientSubnet), 400)
				return
			}
			if ipv4 := ednsClientAddress.To4(); ipv4 != nil {
				ednsClientFamily = 1
				ednsClientAddress = ipv4
				ednsClientNetmask = 24
			} else {
				ednsClientFamily = 2
				ednsClientNetmask = 48
			}
		} else {
			ednsClientAddress = net.ParseIP(ednsClientSubnet[:slash])
			if ednsClientAddress == nil {
				jsonDNS.FormatError(w, fmt.Sprintf("Invalid argument value: \"edns_client_subnet\" = %q", ednsClientSubnet), 400)
				return
			}
			if ipv4 := ednsClientAddress.To4(); ipv4 != nil {
				ednsClientFamily = 1
				ednsClientAddress = ipv4
			} else {
				ednsClientFamily = 2
			}
			netmask, err := strconv.ParseUint(ednsClientSubnet[slash + 1:], 10, 8)
			if err != nil {
				jsonDNS.FormatError(w, fmt.Sprintf("Invalid argument value: \"edns_client_subnet\" = %q (%s)", ednsClientSubnet, err.Error()), 400)
				return
			}
			ednsClientNetmask = uint8(netmask)
		}
	} else {
		ednsClientAddress = s.findClientIP(r)
		if ednsClientAddress == nil {
			ednsClientNetmask = 0
		} else if ipv4 := ednsClientAddress.To4(); ipv4 != nil {
			ednsClientFamily = 1
			ednsClientAddress = ipv4
			ednsClientNetmask = 24
		} else {
			ednsClientFamily = 2
			ednsClientNetmask = 48
		}
	}

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(name), rrType)
	msg.CheckingDisabled = cd
	opt := new(dns.OPT)
	opt.Hdr.Name = "."
	opt.Hdr.Rrtype = dns.TypeOPT
	opt.SetUDPSize(4096)
	opt.SetDo(true)
	if ednsClientAddress != nil {
		edns0Subnet := new(dns.EDNS0_SUBNET)
		edns0Subnet.Code = dns.EDNS0SUBNET
		edns0Subnet.Family = ednsClientFamily
		edns0Subnet.SourceNetmask = ednsClientNetmask
		edns0Subnet.SourceScope = 0
		edns0Subnet.Address = ednsClientAddress
		opt.Option = append(opt.Option, edns0Subnet)
	}
	msg.Extra = append(msg.Extra, opt)

	resp, err := s.doDNSQuery(msg)
	if err != nil {
		jsonDNS.FormatError(w, fmt.Sprintf("DNS query failure (%s)", err.Error()), 503)
		return
	}
	respJson := jsonDNS.Marshal(resp)
	respStr, err := json.Marshal(respJson)
	if err != nil {
		log.Println(err)
		jsonDNS.FormatError(w, fmt.Sprintf("DNS packet parse failure (%s)", err.Error()), 500)
		return
	}

	if respJson.HaveTTL {
		if ednsClientSubnet != "" {
			w.Header().Set("Cache-Control", "public, max-age=" + strconv.Itoa(int(respJson.LeastTTL)))
		} else {
			w.Header().Set("Cache-Control", "private, max-age=" + strconv.Itoa(int(respJson.LeastTTL)))
		}
		w.Header().Set("Expires", respJson.EarliestExpires.Format(http.TimeFormat))
	}
	if respJson.Status == dns.RcodeServerFailure {
		w.WriteHeader(503)
	}
	w.Write(respStr)
}

func (s *Server) findClientIP(r *http.Request) net.IP {
	XForwardedFor := r.Header.Get("X-Forwarded-For")
	if XForwardedFor != "" {
		for _, addr := range strings.Split(XForwardedFor, ",") {
			addr = strings.TrimSpace(addr)
			ip := net.ParseIP(addr)
			if jsonDNS.IsGlobalIP(ip) {
				return ip
			}
		}
	}
	XRealIP := r.Header.Get("X-Real-IP")
	if XRealIP != "" {
		addr := strings.TrimSpace(XRealIP)
		ip := net.ParseIP(addr)
		if jsonDNS.IsGlobalIP(ip) {
			return ip
		}
	}
	remoteAddr, err := net.ResolveTCPAddr("tcp", r.RemoteAddr)
	if err != nil {
		return nil
	}
	if ip := remoteAddr.IP; jsonDNS.IsGlobalIP(ip) {
		return ip
	}
	return nil
}

func (s *Server) doDNSQuery(msg *dns.Msg) (resp *dns.Msg, err error) {
	num_servers := len(s.conf.Upstream)
	for i := uint(0); i < s.conf.Tries; i++ {
		server := s.conf.Upstream[rand.Intn(num_servers)]
		if !s.conf.TCPOnly {
			resp, _, err = s.udpClient.Exchange(msg, server)
			if err == dns.ErrTruncated {
				log.Println(err)
				resp, _, err = s.tcpClient.Exchange(msg, server)
			}
		} else {
			resp, _, err = s.tcpClient.Exchange(msg, server)
		}
		if err == nil {
			return
		}
		log.Println(err)
	}
	return
}
