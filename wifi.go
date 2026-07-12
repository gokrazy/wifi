// wifi is a daemon that tries joining a pre-configured WiFi network.
//
// Example:
//
//	Create a WiFi configuration file,
//	either via https://github.com/gokrazy/breakglass,
//	or by mounting the SD card on the host:
//	# echo '{"ssid": "I/O Tee"}' > /perm/wifi.json
//
//	Include the wifi package in your gokr-packer command:
//	% gokr-packer -update=yes \
//	  github.com/gokrazy/breakglass \
//	  github.com/gokrazy/wifi
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gokrazy/gokrazy"
	"github.com/gokrazy/internal/iface"
	"github.com/mdlayher/wifi"
	"golang.org/x/sys/unix"
)

// worldRegulatoryRegion is a restrictive, safe default if the wireless regulatory region is not known.
const worldRegulatoryRegion = "00"

type wifiConfig struct {
	SSID   string `json:"ssid"`
	PSK    string `json:"psk"`
	Region string `json:"region"`
}

type wifiCtx struct {
	// config
	cl         *wifi.Client
	interfaces []*wifi.Interface
	cfg        *wifiConfig

	// state
	dhcpClientMu   sync.Mutex
	dhcpClient     *exec.Cmd
	connectedSince time.Duration
}

const configFileName = "wifi.json"
const configFileDebounceDuration = 200 * time.Millisecond

func (w *wifiCtx) disconnectAll() {
	for _, intf := range w.interfaces {
		if err := w.cl.Disconnect(intf); err != nil {
			log.Printf("disconnect: %v", err)
		}
	}
	w.dhcpClientMu.Lock()
	if w.dhcpClient != nil && w.dhcpClient.Process != nil {
		w.dhcpClient.Process.Kill()
	}
	w.dhcpClient = nil
	w.dhcpClientMu.Unlock()
}

func (w *wifiCtx) control1() error {
Interface:
	for _, intf := range w.interfaces {
		stationInfos, err := w.cl.StationInfo(intf)
		if err != nil && !errors.Is(err, os.ErrNotExist) /* not connected */ {
			return err
		}
		for _, sta := range stationInfos {
			if bytes.Equal(sta.HardwareAddr, net.HardwareAddr{}) {
				continue
			}
			w.dhcpClientMu.Lock()
			log.Printf("connected to %v for %v, signal %v",
				sta.HardwareAddr,
				sta.Connected,
				sta.Signal)
			if sta.Connected < w.connectedSince {
				// reconnected. restart dhcp client
				if w.dhcpClient != nil && w.dhcpClient.Process != nil {
					w.dhcpClient.Process.Kill()
				}
				w.dhcpClient = nil
			}
			if w.dhcpClient != nil {
				w.connectedSince = sta.Connected
				w.dhcpClientMu.Unlock()
				continue Interface
			}
			w.dhcpClient = exec.Command("/gokrazy/dhcp", "-interface=wlan0")
			w.dhcpClient.SysProcAttr = &syscall.SysProcAttr{
				// When the wifi process dies, make the kernel send a SIGTERM to
				// the dhcp process, too. The bake CI test runner uses
				// exec.CommandContext("wifi") which sends SIGKILL, so trying to
				// clean up the dhcp process from within wifi is fruitless.
				Pdeathsig: syscall.SIGTERM,
			}
			w.dhcpClient.Stdout = os.Stdout
			w.dhcpClient.Stderr = os.Stderr
			log.Printf("starting %v", w.dhcpClient.Args)
			if err := w.dhcpClient.Start(); err != nil {
				log.Printf("dhcp start failed: %v", err)
				w.dhcpClient = nil
				w.dhcpClientMu.Unlock()
				continue Interface
			}
			dhcpClient := w.dhcpClient
			go func() {
				if err := dhcpClient.Wait(); err != nil {
					log.Printf("dhcp process failed: %v", err)
				}
				w.dhcpClientMu.Lock()
				if w.dhcpClient == dhcpClient {
					w.dhcpClient = nil
				}
				w.dhcpClientMu.Unlock()
			}()
			w.connectedSince = sta.Connected
			w.dhcpClientMu.Unlock()
			continue Interface
		}

		// disconnected, ensure dhcp client is stopped:
		w.dhcpClientMu.Lock()
		if w.dhcpClient != nil && w.dhcpClient.Process != nil {
			w.dhcpClient.Process.Kill()
		}
		w.dhcpClient = nil
		w.connectedSince = 0
		w.dhcpClientMu.Unlock()

		// Interface is not associated with station, try connecting:
		if w.cfg.PSK != "" {
			if err := w.cl.ConnectWPAPSK(intf, w.cfg.SSID, w.cfg.PSK); err != nil {
				// -EALREADY means already connected, but misleadingly
				// stringifies to “operation already in progress”
				log.Printf("could not connect: %v", err)
			} else {
				log.Printf("connecting to SSID %q...", w.cfg.SSID)
			}
		} else {
			if err := w.cl.Connect(intf, w.cfg.SSID); err != nil {
				// -EALREADY means already connected, but misleadingly
				// stringifies to “operation already in progress”
				log.Printf("could not connect: %v", err)
			} else {
				log.Printf("connecting to SSID %q...", w.cfg.SSID)
			}
		}
	}
	return nil
}

var release = func() string {
	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		fmt.Fprintf(os.Stderr, "minitrd: %v\n", err)
		os.Exit(1)
	}
	return string(uts.Release[:bytes.IndexByte(uts.Release[:], 0)])
}()

func loadModule(mod string) error {
	f, err := os.Open(filepath.Join("/lib/modules", release, mod))
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.FinitModule(int(f.Fd()), "", 0); err != nil {
		if err != unix.EEXIST &&
			err != unix.EBUSY &&
			err != unix.ENODEV &&
			err != unix.ENOENT {
			return fmt.Errorf("FinitModule(%v): %v", mod, err)
		}
	}
	return nil
}

// configPaths lists the well-known config file locations in priority order.
// /perm/wifi.json is preferred (user-writable), /etc/wifi.json is the read-only system-wide fallback.
var configPaths = []struct {
	dir  string
	file string
}{
	{"/perm", filepath.Join("/perm", configFileName)},
	{"/etc", filepath.Join("/etc", configFileName)},
}

// readConfig tries each config path in priority order.
// Returns (nil, nil) when no config file exists.
func readConfig() (*wifiConfig, error) {
	for _, p := range configPaths {
		b, err := os.ReadFile(p.file)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		var cfg wifiConfig
		if err := json.Unmarshal(b, &cfg); err != nil {
			return nil, err
		}
		return &cfg, nil
	}
	return nil, nil
}

// watchConfigFiles monitors the well-known config file parent directories for changes.
// We watch directories rather than files because editors do atomic rename-into-place
// writes, which would remove a watch on the file itself.
// Events are debounced (200ms) to avoid reading a half-written file.
func watchConfigFiles(notify chan<- struct{}) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("error creating config file watcher (fsnotify): %v (config watching disabled)", err)
		return
	}
	defer watcher.Close()

	// Watch each parent directory for config file changes.
	// Directories that don't exist (e.g. /perm/ not yet mounted) are skipped gracefully.
	watched := 0
	for _, p := range configPaths {
		if err := watcher.Add(p.dir); err != nil {
			log.Printf("error watching config file parent directory %s (fsnotify): %v (config watching disabled)", p.dir, err)
			continue
		}
		watched++
	}
	if watched == 0 {
		log.Printf("no config file parent directories could be watched (config watching disabled)")
		return
	}

	var debounce *time.Timer
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Given we watch parent directories, only react to events for configured config files, ignore others.
			if filepath.Base(event.Name) != configFileName {
				continue
			}
			// Only react to events that indicate a change we are interested in.
			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) && !event.Has(fsnotify.Remove) && !event.Has(fsnotify.Rename) {
				continue
			}
			// If a previous debounce timer is already running, cancel it.
			// This ensures that rapid successive file-change events don't each trigger a notification.
			if debounce != nil {
				debounce.Stop()
			}
			// Start a new timer and schedule a function to run after time is elapsed.
			// Every new file event resets this timer, so the actual notification
			// only fires once activity has "settled" for at least the debounce duration.
			debounce = time.AfterFunc(configFileDebounceDuration, func() {
				// When the timer fires, it tries to send on the notify channel.
				// The select with a default case makes this non-blocking.
				// If nobody is ready to receive (or the channel buffer is full),
				// the signal is silently dropped rather than blocking the goroutine.
				select {
				case notify <- struct{}{}:
					log.Printf("config file change detected: %s (event: %v)", event.Name, event.Op)
				default:
				}
			})
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("fsnotify error: %v", err)
		}
	}
}

func logic() error {
	var (
		disconnect = flag.Bool("disconnect",
			false,
			"instead of connecting to a WiFi network, disconnect the interface and exit")

		ssid = flag.String("ssid",
			"",
			"if non-empty, the ssid of the WiFi network to connect to. if empty, /perm/"+configFileName+" or /etc/"+configFileName+" will be used")

		psk = flag.String("psk",
			"",
			"if non-empty, the psk of the WiFi network to connect to. if empty, /perm/"+configFileName+" or /etc/"+configFileName+" will be used")

		region = flag.String("region",
			"",
			"if non-empty, the wireless regulatory region (ISO 3166-1 alpha-2). if empty, /perm/"+configFileName+" or /etc/"+configFileName+" will be used")

		watchConfig = flag.Bool("watch-config",
			false,
			"if set, watch /perm/"+configFileName+" and /etc/"+configFileName+" for changes and reload the configuration dynamically")
	)
	flag.Parse()

	if *watchConfig && (*ssid != "" || *psk != "" || *region != "" || *disconnect) {
		return fmt.Errorf("-watch-config cannot be combined with -ssid, -psk, -region, or -disconnect")
	}

	var cfg *wifiConfig
	if *ssid != "" || *disconnect {
		cfg = &wifiConfig{
			SSID:   *ssid,
			PSK:    *psk,
			Region: *region,
		}
	} else {
		var err error
		cfg, err = readConfig()
		if err != nil {
			return err
		}
		if cfg == nil {
			if *watchConfig {
				log.Printf("no config file found, waiting for /perm/" + configFileName + " or /etc/" + configFileName + " to appear")
			} else {
				gokrazy.DontStartOnBoot()
				return fmt.Errorf("no config file found")
			}
		}
	}

	if cfg != nil && cfg.Region == "" {
		cfg.Region = worldRegulatoryRegion
	}

	// modprobe the brcmfmac driver
	for _, mod := range []string{
		"kernel/drivers/net/wireless/broadcom/brcm80211/brcmutil/brcmutil.ko",
		"kernel/drivers/net/wireless/broadcom/brcm80211/brcmfmac/brcmfmac.ko",
		"kernel/drivers/net/wireless/broadcom/brcm80211/brcmfmac/wcc/brcmfmac-wcc.ko",
		"kernel/drivers/net/wireless/broadcom/brcm80211/brcmfmac/cyw/brcmfmac-cyw.ko",
	} {
		if err := loadModule(mod); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	cl, err := wifi.New()
	if err != nil {
		return err
	}
	interfaces, err := cl.Interfaces()
	if err != nil {
		return err
	}
	if len(interfaces) == 0 {
		return fmt.Errorf("no interfaces found")
	}

	if *disconnect {
		for _, intf := range interfaces {
			if err := cl.Disconnect(intf); err != nil {
				return err
			}
		}
		return nil
	}

	if cfg != nil {
		if err := applyRegulatoryRegion(cl, cfg.Region); err != nil {
			return err
		}
	}

	w := &wifiCtx{
		cl:         cl,
		interfaces: interfaces,
		cfg:        cfg,
	}

	cs, err := iface.NewConfigSocket("wlan0")
	if err != nil {
		return fmt.Errorf("config socket: %v", err)
	}
	defer cs.Close()

	b, err := os.ReadFile("/sys/class/net/wlan0/address")
	if err != nil {
		return fmt.Errorf("reading /sys/class/net/wlan0/address: %v", err)
	}
	log.Printf("wlan0 MAC address is %s", strings.TrimSpace(string(b)))

	// Ensure the interface is up so that we can send DHCP packets.
	if err := cs.Up(); err != nil {
		log.Printf("setting link wlan0 up: %v", err)
	}

	// Watch for config files events if requested.
	var configNotify <-chan struct{}
	if *watchConfig {
		ch := make(chan struct{}, 1)
		configNotify = ch
		go watchConfigFiles(ch)
	}

	const controlLoopFrequency = 15 * time.Second
	ticker := time.NewTicker(controlLoopFrequency)
	defer ticker.Stop()

	for {
		if w.cfg != nil {
			if err := w.control1(); err != nil {
				log.Printf("control1: %v", err)
			}
		}

		select {
		case <-ticker.C: // control loop tick
		case <-configNotify: // config file changed
			newCfg, err := readConfig()
			if err != nil {
				log.Printf("error reading config file: %v (keeping previous config)", err)
				continue
			}
			if newCfg == nil {
				log.Printf("config file was removed, will disconnect (if connected)")
				w.cfg = nil
				w.disconnectAll()
				continue
			}
			if newCfg.Region == "" {
				newCfg.Region = worldRegulatoryRegion
			}
			if hasConfigChanged(w.cfg, newCfg) {
				log.Print("wifi config changed, will use new config")
				if w.cfg != nil {
					w.disconnectAll()
				}
				w.cfg = newCfg
				if err := applyRegulatoryRegion(w.cl, w.cfg.Region); err != nil {
					log.Printf("could not apply regulatory region: %v", err)
				}
			}
		}
	}
}

// applyRegulatoryRegion applies the regulatory region to the WiFi client.
func applyRegulatoryRegion(cl *wifi.Client, region string) error {
	if err := cl.ReloadRegulatoryDatabase(); err != nil {
		return fmt.Errorf("failed to reload the wireless regulatory database: %w", err)
	}
	if err := cl.SetRegulatoryRegion(region, wifi.RegulatoryHintUser); err != nil {
		return fmt.Errorf("failed to set %s as the regulatory region: %w", region, err)
	}
	log.Printf("requested wireless regulatory region %s", region)
	return nil
}

// hasConfigChanged checks if the configuration has changed.
func hasConfigChanged(cfg *wifiConfig, newCfg *wifiConfig) bool {
	return cfg == nil ||
		newCfg.SSID != cfg.SSID ||
		newCfg.PSK != cfg.PSK ||
		newCfg.Region != cfg.Region
}

func main() {
	if err := logic(); err != nil {
		log.Fatal(err)
	}
}
