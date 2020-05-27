// wifi is a daemon that tries joining a pre-configured WiFi network.
//
// Example:
//   Create a WiFi configuration file,
//   either via https://github.com/gokrazy/breakglass,
//   or by mounting the SD card on the host:
//   # echo '{"ssid": "I/O Tee"}' > /perm/wifi.json
//
//   Include the wifi package in your gokr-packer command:
//   % gokr-packer -update=yes \
//     github.com/gokrazy/breakglass \
//     github.com/gokrazy/wifi
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gokrazy/gokrazy"
	"github.com/gokrazy/internal/iface"
	"github.com/mdlayher/wifi"
)

type wifiConfig struct {
	SSID string `json:"ssid"`
}

type wifiCtx struct {
	// config
	cl         *wifi.Client
	interfaces []*wifi.Interface
	cfg        *wifiConfig

	// state
	dhcpClient     *exec.Cmd
	connectedSince time.Duration
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
			log.Printf("connected to %v for %v, signal %v",
				sta.HardwareAddr,
				sta.Connected,
				sta.Signal)
			if sta.Connected < w.connectedSince {
				// reconnected. restart dhcp client
				if w.dhcpClient.Process != nil {
					w.dhcpClient.Process.Kill()
				}
				w.dhcpClient = nil
			}
			if w.dhcpClient != nil {
				continue Interface
			}
			w.dhcpClient = exec.Command("/gokrazy/dhcp", "-interface=wlan0")
			w.dhcpClient.Stdout = os.Stdout
			w.dhcpClient.Stderr = os.Stderr
			log.Printf("starting %v", w.dhcpClient.Args)
			w.dhcpClient.Start()
			continue Interface
		}

		// disconnected, ensure dhcp client is stopped:
		if w.dhcpClient != nil && w.dhcpClient.Process != nil {
			w.dhcpClient.Process.Kill()
		}
		w.dhcpClient = nil

		// Interface is not associated with station, try connecting:
		if err := w.cl.Connect(intf, w.cfg.SSID); err != nil {
			// -EALREADY means already connected, but misleadingly
			// stringifies to “operation already in progress”
			log.Printf("could not connect: %v", err)
		} else {
			log.Printf("connecting to SSID %q...", w.cfg.SSID)
		}
	}
	return nil
}

func feedFirmware() error {
	for _, fw := range []struct {
		loaderPath   string
		firmwarePath string
	}{
		{
			loaderPath:   "brcm!brcmfmac43455-sdio.bin",
			firmwarePath: "third_party/firmware-nonfree/brcmfmac43455-sdio.bin",
		},

		{
			loaderPath:   "brcm!brcmfmac43455-sdio.raspberrypi,4-model-b.txt",
			firmwarePath: "third_party/firmware-nonfree/brcmfmac43455-sdio.txt",
		},

		{
			loaderPath:   "brcm!brcmfmac43455-sdio.txt",
			firmwarePath: "third_party/firmware-nonfree/brcmfmac43455-sdio.txt",
		},

		{
			loaderPath:   "brcm!brcmfmac43455-sdio.clm_blob",
			firmwarePath: "third_party/firmware-nonfree/brcmfmac43455-sdio.clm_blob",
		},
	} {
		loadingFn := filepath.Join("/sys/class/firmware", fw.loaderPath, "loading")
		if err := ioutil.WriteFile(loadingFn, []byte("1\n"), 0644); err != nil {
			if os.IsNotExist(err) {
				continue // already loaded or loading timed out
			}
			return err
		}

		b := firmware[fw.firmwarePath]
		log.Printf("feeding firmware (%d bytes) to brcmfmac via sysfs/%s",
			len(b),
			fw.loaderPath)
		if err := ioutil.WriteFile(
			filepath.Join("/sys/class/firmware/", fw.loaderPath, "data"),
			b,
			0644); err != nil {
			return err
		}

		if err := ioutil.WriteFile(loadingFn, []byte("0\n"), 0644); err != nil {
			return err
		}
	}
	return nil
}

func logic() error {
	b, err := ioutil.ReadFile("/perm/wifi.json")
	if err != nil {
		if os.IsNotExist(err) {
			// No config file? Nothing to do!
			gokrazy.DontStartOnBoot()
		}
		return err
	}
	var cfg wifiConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}

	// If the brcmfmac driver is asking,
	// feed it the firmware via sysfs.
	// This assumes we are running early at boot,
	// before the sysfs firmware load times out,
	// which is the case for gokrazy packages :)
	if err := feedFirmware(); err != nil {
		return fmt.Errorf("feeding firmware to brcmfmac driver: %v", err)
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

	w := &wifiCtx{
		cl:         cl,
		interfaces: interfaces,
		cfg:        &cfg,
	}

	cs, err := iface.NewConfigSocket("wlan0")
	if err != nil {
		return fmt.Errorf("config socket: %v", err)
	}
	defer cs.Close()

	// Ensure the interface is up so that we can send DHCP packets.
	if err := cs.Up(); err != nil {
		log.Printf("setting link wlan0 up: %v", err)
	}

	const controlLoopFrequency = 15 * time.Second
	for {
		if err := w.control1(); err != nil {
			log.Printf("control1: %v", err)
		}
		time.Sleep(controlLoopFrequency)
	}
	return nil
}

func main() {
	if err := logic(); err != nil {
		log.Fatal(err)
	}
}
