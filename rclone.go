// Sync files and directories to and from local and remote object stores
//
// Nick Craig-Wood <nick@craig-wood.com>
package main

import (
	"crypto/tls"
	_ "github.com/rclone/rclone/backend/all" // import all backends
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all" // import all commands
	"github.com/rclone/rclone/fs"
	_ "github.com/rclone/rclone/lib/plugin" // import plugins
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

func bToMb(b uint64) uint64 {
	return b / 1024 / 1024
}

func PrintMemUsage() {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// For info on each, see: https://golang.org/pkg/runtime/#MemStats
	log.Printf("Alloc = %v MiB\tTotalAlloc = %v MiB\tSys = %v MiB\tGoroutines = %v\tNumGC = %v",
		bToMb(m.Alloc),
		bToMb(m.TotalAlloc),
		bToMb(m.Sys),
		runtime.NumGoroutine(),
		m.NumGC)
}

func printMem() {
	log.Printf("Starting pprof server")
	go http.ListenAndServe("0.0.0.0:49315", nil)

	go func() {
		ticker := time.NewTicker(time.Minute * 1)
		for {
			PrintMemUsage()
			<-ticker.C
		}
	}()
	ticker := time.NewTicker(time.Second * 5)
	for {
		//start := time.Now()
		runtime.GC()
		debug.FreeOSMemory()
		//log.Printf("GC & scvg use %s", time.Now().Sub(start))
		<-ticker.C
	}
}

func main() {
	http.DefaultClient.Transport = &http.Transport{
		DisableKeepAlives:  false,
		DisableCompression: true,
	}
	fs.Config.StreamingUploadCutoff = fs.SizeSuffix(0)
	fs.Config.IgnoreChecksum = true
	rand.Seed(time.Now().UnixNano())
	/*go func() {
		t := time.NewTicker(time.Second * 3)
		for {
			fs.Config.UserAgent = fmt.Sprintf(
				"Mozilla/5.0 (Linux; Android 6.0; Nexus 5 Build/MRA58N) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/75.0.%d.%d Mobile Safari/537.36",
				rand.Intn(7000), rand.Intn(1000))
			<-t.C
		}
	}()*/
	fs.Config.UserAgent = "google-api-go-client/0.5"
	if false {
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		addrReplace := func(addr string) string {
			if addr == "www.googleapis.com:443" {
				//addr = "216.58.198.206:443"
				addrs := []string{"www.googleapis.com:443", "private.googleapis.com:443", "10.224.1.3:19999", "10.224.1.3:19999"}
				//addrs := []string{"10.224.1.3:19999"}
				addr = addrs[rand.Intn(len(addrs))]
			}
			return addr
		}
		dialTls :=
			func(network, addr string) (conn net.Conn, err error) {
				addr = addrReplace(addr)
				if !strings.HasSuffix(addr, ":443") {
					return dialer.Dial(network, addr)
				}
				c, err := tls.Dial(network, addr, &tls.Config{InsecureSkipVerify: true})
				if err != nil {
					//log.Println("DialTls Err:", err)
					return nil, err
				}
				//log.Println("doing handshake")
				err = c.Handshake()
				if err != nil {
					return c, err
				}
				//log.Println(c.RemoteAddr())
				return c, c.Handshake()
			}
		//dialTls := nil
		http.DefaultTransport = &http.Transport{
			DisableKeepAlives:  false, // disable keep alive to avoid connection reset
			DisableCompression: true,
			IdleConnTimeout:    time.Second * 10,
			ForceAttemptHTTP2:  false,
			DialTLS:            dialTls,
			//DialContext: func(ctx context.Context, network, addr string) (conn net.Conn, err error) {
			/*ipaddr := "10.168.1." + strconv.Itoa(100 + rand.Intn(20))
			netaddr, _ := net.ResolveIPAddr("ip", ipaddr)
			return (&net.Dialer{
				LocalAddr: &net.TCPAddr{
					IP: netaddr.IP,
				},
				Timeout:   8 * time.Second,
			}).DialContext(ctx, network, addr)*/
			/*if addr == "www.googleapis.com:443" {
				//addr = "216.58.198.206:443"
				addrs := []string{"private.googleapis.com", "www.googleapis.com:443"}
				rand.Intn(len(addrs))
				addr = "216.58.198.206:443"
			}
			return dialer.DialContext(ctx, network, addr)*/
			//},
			//ForceAttemptHTTP2:      true,
		}
	}

	go printMem()
	cmd.Main()
}
