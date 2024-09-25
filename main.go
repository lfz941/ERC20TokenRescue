package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/lfz941/ERC20TokenRescue/erc20"
	"github.com/lfz941/ERC20TokenRescue/input"
)

type RPCCli struct {
	RPC string
	Cli *ethclient.Client
}

var defaultRPCEndpoints = []string{
	"https://rpc.ankr.com/eth",
	"https://ethereum.rpc.subquery.network/public",
	"https://ethereum-rpc.publicnode.com",
	"https://cloudflare-eth.com/",
	"https://1rpc.io/eth",
	"https://eth-mainnet.public.blastapi.io",
	"https://eth.drpc.org",
	"https://gateway.tenderly.co/public/mainnet",
	"https://eth-mainnet-public.unifra.io",
}

var (
	masterRpcCli *ethclient.Client
)

var (
	rpcEndpoints  = flag.String("nodes", "", "multiple nodes can be splited with comma. there are default eth rpc endpoints if nodes is empty")
	token         = flag.String("token", "0xec53bf9167f50cdeb3ae105f56099aaab9061f83", "erc20 token addr")
	privateKeyHex = flag.String("priv", "", "priv key") // TODO change to interactive input
	targetAddrHex = flag.String("to", "", "the target address that erc20 token should transfer to")
	startAt       = flag.Int64("begin", 0, "schedule the begin timestamp, unit(s)")
)

func main() {
	flag.Parse()

	// check flag value
	if *privateKeyHex == "" {
		panic("the priv key shouldn't be empty")
	}
	if *targetAddrHex == "" {
		panic("the target address shouldn't be empty")
	}
	privateKey, err := crypto.HexToECDSA(*privateKeyHex)
	if err != nil {
		panic(err)
	}
	publicKey := privateKey.Public()
	publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
	if !ok {
		panic("invalid priv key")
	}
	accAddr := crypto.PubkeyToAddress(*publicKeyECDSA)
	targetAddr := common.HexToAddress(*targetAddrHex)

	usedRPCEndpoints := defaultRPCEndpoints
	if *rpcEndpoints != "" {
		usedRPCEndpoints = strings.Split(*rpcEndpoints, ",")
	}

	// init rpcClients
	rpcClients := make([]RPCCli, 0)
	for i, endpoint := range usedRPCEndpoints {
		client := initEthCli(i, endpoint)
		if client != nil {
			rpcClients = append(rpcClients, RPCCli{usedRPCEndpoints[i], client})
		}
	}
	if len(rpcClients) == 0 {
		panic("no valid rpc endpoint")
	}
	masterRpcCli = rpcClients[0].Cli

	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGABRT,
		syscall.SIGKILL,
	)
	defer stop()

	chainID, err := masterRpcCli.ChainID(ctx)
	if err != nil {
		panic(err)
	}
	tokenAddr := common.HexToAddress(*token)
	// init erc20 token abi instance
	erc20TokenIns, err := erc20.NewErc20(tokenAddr, masterRpcCli)
	if err != nil {
		panic(err)
	}
	// get token info
	tokenSymbol, err := erc20TokenIns.Symbol(nil)
	if err != nil {
		panic(err)
	}
	tokenDecimal, err := erc20TokenIns.Decimals(nil)
	if err != nil {
		panic(err)
	}
	bal, err := erc20TokenIns.BalanceOf(nil, accAddr)
	if err != nil {
		panic(err)
	}
	balStr, _ := formatTokenAmtToHumanReadable(bal, int(tokenDecimal), 2)
	slog.With("b", balStr+tokenSymbol).Info("bal info")
	yes, err := input.GetConfirmation(fmt.Sprintf("Please confirm the rescue info: \n chainID: %d \n user: %s \n erc20 token: %s(%s) \n bal: %s \n", chainID.Int64(), accAddr.Hex(), tokenSymbol, tokenAddr.Hex(), balStr), bufio.NewReader(os.Stderr), os.Stderr)
	if err != nil {
		panic(err)
	}
	if !yes {
		return
	}

	now := time.Now().Unix()
	if *startAt > now {
		waitDur := time.Duration(now-*startAt) * time.Second
		fmt.Printf("rescue job was scheduled starting after %v\n", waitDur)
		<-time.After(waitDur)
	}
	fmt.Println("start rescue job now")
	for {
		select {
		case <-ctx.Done():
			slog.Info("process exit")
			return
		default:
			transactOpts, err := bind.NewKeyedTransactorWithChainID(privateKey, chainID)
			if err != nil {
				slog.With("err", err).Error("NewKeyedTransactorWithChainID error")
				continue
			}
			var (
				wg     sync.WaitGroup
				txHash string
			)
			wg.Add(len(rpcClients))
			for i := range rpcClients {
				go func(i int) {
					defer wg.Done()
					erc20Ins, err := erc20.NewErc20(tokenAddr, rpcClients[i].Cli)
					if err != nil {
						slog.With("err", err, "rpc", rpcClients[i].RPC).Error("NewErc20 error")
						return
					}
					tx, err := erc20Ins.Transfer(transactOpts, targetAddr, bal)
					if err != nil {
						slog.With("err", err, "rpc", rpcClients[i].RPC).Error("transfer erc20 token error")
						return
					}
					txHash = tx.Hash().String()
				}(i)
			}
			wg.Wait()
			if txHash != "" {
				slog.With("tx", txHash).Info("transfer erc20 token success")
			}
		}
	}
}

func formatTokenAmtToHumanReadable(amt *big.Int, decimal int, prec int) (string, float64) {
	amtBigFloat := new(big.Float).Quo(new(big.Float).SetInt(amt), big.NewFloat(math.Pow10(decimal)))
	amtStr := amtBigFloat.Text('f', prec)
	amtF, _ := strconv.ParseFloat(amtStr, 64)
	return amtStr, amtF
}

func initEthCli(idx int, rpc string) *ethclient.Client {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ethCli, err := ethclient.DialContext(ctx, rpc)
	if err != nil {
		slog.With("err", err, "idx", idx, "rpc", rpc).Error("dial ether rpc client error")
		return nil
	}
	return ethCli
}
