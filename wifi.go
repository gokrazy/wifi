// wifi is a daemon that tries joining a pre-configured WiFi network.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mdlayher/wifi"
)

func control1(cl *wifi.Client, interfaces []*wifi.Interface) error {
Interface:
	for _, intf := range interfaces {
		stationInfos, err := cl.StationInfo(intf)
		if err != nil && !errors.Is(err, os.ErrNotExist) /* not connected */ {
			return err
		}
		for _, sta := range stationInfos {
			if !bytes.Equal(sta.HardwareAddr, net.HardwareAddr{}) {
				log.Printf("connected to %v for %v, signal %v",
					sta.HardwareAddr,
					sta.Connected,
					sta.Signal)
				// TODO: ensure a dhcp4 client is up for intf.Name
				continue Interface
			}
		}

		// TODO: ensure any dhcp4 clients for intf.Name are killed

		// Interface is not associated with station, try connecting:
		if err := cl.Connect(intf, "I/O Tee"); err != nil {
			// -EALREADY means already connected, but misleadingly
			// stringifies to “operation already in progress”
			log.Printf("could not connect: %v", err)
		} else {
			log.Printf("connecting...")
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
	// TODO: read configuration data

	// If the brcmfmac driver is asking,
	// feed it the firmware via sysfs.
	// This assumes we are running early at boot,
	// before the sysfs firmware load times out,
	// which is the case for gokrazy packages :)
	if err := feedFirmware(); err != nil {
		return fmt.Errorf("feeding firmware to brcmfmac driver: %v", err)
	}

	// TODO: ip link set dev wlan0 up

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
	const controlLoopFrequency = 15 * time.Second
	for {
		if err := control1(cl, interfaces); err != nil {
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
