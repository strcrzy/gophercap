/*
Copyright © 2021 Stamus Networks oss@stamus-networks.com

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/
package extract

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"path"
	"strconv"
	"time"

	"encoding/json"

	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"
	"github.com/google/gopacket/pcapgo"
	"github.com/sirupsen/logrus"
)

const FlowTimeout time.Duration = 600 * 1000000000

func writePacket(handle *pcap.Handle, buf []byte) error {
	if err := handle.WritePacketData(buf); err != nil {
		logrus.Warningf("Failed to write packet: %s\n", err)
		return err
	}
	return nil
}

// IPAddr is for decoding IP values directly to IP objects during JSON decode. net.IP is a wrapper
// around byte array, not integer, so it also handles IPv6 addresses.
type IPAddr struct{ net.IP }

// UnmarshalJSON implements json.Unmarshaler
func (t *IPAddr) UnmarshalJSON(b []byte) error {
	str, err := strconv.Unquote(string(b))
	if err != nil {
		return err
	}
	ip := net.ParseIP(str)
	if ip == nil {
		return fmt.Errorf("Invalid IP: %s", string(b))
	}
	t.IP = ip
	return nil
}

type Tunnel struct {
	SrcIP    IPAddr `json:"src_ip"`
	DestIP   IPAddr `json:"dest_ip"`
	SrcPort  uint16 `json:"src_port"`
	DestPort uint16 `json:"dest_port"`
	Proto    string `json:"proto"`
	Depth    uint8  `json:"depth"`
}

type Event struct {
	Timestamp   string
	CaptureFile string `json:"capture_file"`
	SrcIP       IPAddr `json:"src_ip"`
	DestIP      IPAddr `json:"dest_ip"`
	SrcPort     uint16 `json:"src_port"`
	DestPort    uint16 `json:"dest_port"`
	AppProto    string `json:"app_proto"`
	Proto       string `json:"proto"`
	Tunnel      Tunnel `json:"tunnel"`
}

func openPcapReaderHandle(fName string, bpfFilter string) (*pcap.Handle, error) {
	// Open PCAP file + handle potential BPF Filter
	// TODO maybe use a pcapgo reader
	handleRead, err := pcap.OpenOffline(fName)
	if err != nil {
		return handleRead, err
	}

	if err == nil {
		err = handleRead.SetBPFFilter(bpfFilter)
		if err != nil {
			logrus.Errorf("Invalid BPF Filter: %s", err)
			return handleRead, err
		}
	}
	return handleRead, nil
}

type ExtractPcapConfig struct {
	PcapLogDirectory string
	OutputName       string
	EventPath        string
	FileFormat       string
	SkipBpf          bool
}

/*
Extract a pcap file for a given flow
*/
func ExtractPcapFile(config ExtractPcapConfig) error {
	/* open event file */
	eventFile, err := os.Open(config.EventPath)
	if err != nil {
		return err
	}
	defer eventFile.Close()

	eventPathstring, err := ioutil.ReadAll(eventFile)
	if err != nil {
		return err
	}

	var event Event
	err = json.Unmarshal(eventPathstring, &event)
	if err != nil {
		return errors.New("Can't parse JSON in " + config.EventPath)
	}

	if len(event.CaptureFile) > 0 {
		filename := path.Join(config.PcapLogDirectory, event.CaptureFile)
		_, err := os.Stat(filename)
		if os.IsNotExist(err) {
			return err
		}
		logrus.Debugf("Starting from file %s", filename)
	}

	if event.Tunnel.Depth != 0 {
		logrus.Debugf("Tunnel: %s <-%s-> %s\n", event.Tunnel.SrcIP, event.Tunnel.Proto, event.Tunnel.DestIP)
	}
	logrus.Debugf("Flow: %s <-%s:%s-> %s\n", event.SrcIP, event.Proto, event.AppProto, event.DestIP)
	eventFlowPair, err := buildEndpoints(event)
	if err != nil {
		return err
	}

	pcapFileList := NewPcapFileList(config.PcapLogDirectory, event, config.FileFormat)
	if pcapFileList == nil {
		return errors.New("Problem when building pcap file list")
	}

	bpfFilter := ""
	if config.SkipBpf != true {
		bpfFilter, err = buildBPF(event)
		if err != nil {
			logrus.Warning(err)
		}
	}

	// Open up a second pcap handle for packet writes.
	outfile, err := os.Create(config.OutputName)
	if err != nil {
		logrus.Error("Can't open pcap output file: ", err)
		return err
	}
	defer outfile.Close()

	handleWrite := pcapgo.NewWriter(outfile)
	handleWrite.WriteFileHeader(65536, layers.LinkTypeEthernet) // new file, must do this.
	if err != nil {
		logrus.Error("Can't write to output file: ", err)
		return err
	}

	start := time.Now()
	var pktCount uint64 = 0
	/* FIXME we can do better here */
	var lastTimestamp time.Time = time.Now()
	var firstTimestamp time.Time = time.Now()

	fName, err := pcapFileList.GetNext()
	if err != nil {
		switch err.(type) {
		case ErrOutOfFiles, *ErrOutOfFiles:
			logrus.Debugf("Expected at least one file: %s\n", err)
			return nil
		default:
			return err
		}
	}
	/*
		Loop over pcap file starting with the one specified in the event
		If timestamp of first packet > lastTimestamp of flow + flow_timeout then
		we can consider we are at the last pcap
	*/
	for len(event.CaptureFile) == 0 || firstTimestamp.Before(lastTimestamp.Add(FlowTimeout)) {
		filePkt := 0
		logrus.Debugf("Reading packets from %s", fName)
		handleRead, err := openPcapReaderHandle(fName, bpfFilter)
		defer handleRead.Close()
		if err != nil {
			logrus.Warningln("This was fast")
			break
		}

		// Loop over packets and write them
		for {
			data, ci, err := handleRead.ReadPacketData()

			switch {
			case err == io.EOF:
				logrus.Debugf("Extracted %d packet(s) from pcap file %s", filePkt, fName)
				goto NextFile
			case err != nil:
				logrus.Warningf("Failed to read packet %d: %s\n", pktCount, err)
			default:
				if config.SkipBpf == true || event.Tunnel.Depth > 0 {
					if filterTunnel(data, *eventFlowPair, event) {
						handleWrite.WritePacket(ci, data)
						pktCount++
						lastTimestamp = ci.Timestamp
					}
				} else {
					handleWrite.WritePacket(ci, data)
					pktCount++
					filePkt++
				}
			}
		}
	NextFile:
		/* Open new pcap to see the beginning */
		fName, err = pcapFileList.GetNext()
		if err != nil {
			logrus.Debugln(err)
			break
		}
		handleTest, err := openPcapReaderHandle(fName, bpfFilter)
		if err != nil {
			break
		}
		_, ci, err := handleRead.ReadPacketData()
		firstTimestamp = ci.Timestamp
		handleTest.Close()
	}
	logrus.Infof("Finished in %s\n", time.Since(start))
	logrus.Infof("Written %d packet(s)\n", pktCount)

	return nil
}
