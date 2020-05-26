// wifi is a daemon that tries joining a pre-configured WiFi network.
package main

import (
	"bytes"
	"errors"
	"log"
	"net"
	"os"
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

func logic() error {
	// TODO: read configuration data

	// TODO: feed firmware into the driver via sysfs
	// assumes we are running early at boot
	// (before the sysfs firmware load times out),
	// but that assumption holds for gokrazy packages :)

	// TODO: ip link set dev wlan0 up

	cl, err := wifi.New()
	if err != nil {
		return err
	}
	interfaces, err := cl.Interfaces()
	if err != nil {
		return err
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
