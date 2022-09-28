package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cosmos/cosmos-sdk/client/input"
	golog "github.com/ipfs/go-log"
	"gitlab.com/thorchain/binance-sdk/common/types"

	"github.com/akildemir/go-tss/common"
	"github.com/akildemir/go-tss/conversion"
	"github.com/akildemir/go-tss/p2p"
	"github.com/akildemir/go-tss/storage"
	"github.com/akildemir/go-tss/tss"
)

var (
	help       bool
	logLevel   string
	pretty     bool
	baseFolder string
	tssAddr    string
)

func main() {
	// Parse the cli into configuration structs
	tssConf, p2pConf := parseFlags()
	if help {
		flag.PrintDefaults()
		return
	}
	// Setup logging
	golog.SetAllLoggers(golog.LevelInfo)
	_ = golog.SetLogLevel("tss-lib", "INFO")
	common.InitLog(logLevel, pretty, "tss_service")

	// Setup Bech32 Prefixes
	conversion.SetupBech32Prefix()
	// this is only need for the binance library
	if os.Getenv("NET") == "testnet" || os.Getenv("NET") == "mocknet" {
		types.Network = types.TestNetwork
	}
	// Read stdin for the private key
	inBuf := bufio.NewReader(os.Stdin)
	priKeyBytes, err := input.GetPassword("input node secret key:", inBuf)
	if err != nil {
		fmt.Printf("error in get the secret key: %s\n", err.Error())
		return
	}
	priKey, err := conversion.GetPriKey(priKeyBytes)
	if err != nil {
		log.Fatal(err)
	}

	// set up tss comms
	stateManager, err := storage.NewFileStateMgr(baseFolder)
	if err != nil {
		log.Fatal(err)
	}
	var bootstrapPeers p2p.AddrList
	savedPeers, err := stateManager.RetrieveP2PAddresses()
	if err != nil {
		bootstrapPeers = p2p.AddrList(p2pConf.BootstrapPeers)
	} else {
		bootstrapPeers = savedPeers
		bootstrapPeers = append(bootstrapPeers, p2p.AddrList(p2pConf.BootstrapPeers)...)
	}
	comm, err := p2p.NewCommunication(p2pConf.RendezvousString, bootstrapPeers, p2pConf.Port, p2pConf.ExternalIP)
	if err != nil {
		fmt.Errorf("fail to create communication layer: %w", err)
		return
	}

	// init tss module
	tss, err := tss.NewTss(
		comm,
		priKey,
		baseFolder,
		tssConf,
		nil,
	)
	if nil != err {
		log.Fatal(err)
	}
	s := NewTssHttpServer(tssAddr, tss)
	go func() {
		if err := s.Start(); err != nil {
			fmt.Println(err)
		}
	}()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	fmt.Println("stop ")
	fmt.Println(s.Stop())
}

// parseFlags - Parses the cli flags
func parseFlags() (tssConf common.TssConfig, p2pConf p2p.Config) {
	// we setup the configure for the general configuration
	flag.StringVar(&tssAddr, "tss-port", "127.0.0.1:8080", "tss port")
	flag.BoolVar(&help, "h", false, "Display Help")
	flag.StringVar(&logLevel, "loglevel", "info", "Log Level")
	flag.BoolVar(&pretty, "pretty-log", false, "Enables unstructured prettified logging. This is useful for local debugging")
	flag.StringVar(&baseFolder, "home", "", "home folder to store the keygen state file")

	// we setup the Tss parameter configuration
	flag.DurationVar(&tssConf.KeyGenTimeout, "gentimeout", 30*time.Second, "keygen timeout")
	flag.DurationVar(&tssConf.KeySignTimeout, "signtimeout", 30*time.Second, "keysign timeout")
	flag.DurationVar(&tssConf.PreParamTimeout, "preparamtimeout", 5*time.Minute, "pre-parameter generation timeout")
	flag.BoolVar(&tssConf.EnableMonitor, "enablemonitor", true, "enable the tss monitor")

	// we setup the p2p network configuration
	flag.StringVar(&p2pConf.RendezvousString, "rendezvous", "Asgard",
		"Unique string to identify group of nodes. Share this with your friends to let them connect with you")
	flag.IntVar(&p2pConf.Port, "p2p-port", 6668, "listening port local")
	flag.StringVar(&p2pConf.ExternalIP, "external-ip", "", "external IP of this node")
	flag.Var(&p2pConf.BootstrapPeers, "peer", "Adds a peer multiaddress to the bootstrap list")
	flag.Parse()
	return
}
