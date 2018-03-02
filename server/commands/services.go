package commands

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/urfave/cli.v1"

	ethUtils "github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/console"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"

	"github.com/tendermint/tendermint/node"
	"github.com/tendermint/tendermint/proxy"
	"github.com/tendermint/tendermint/types"
	"github.com/tendermint/abci/server"
	abcitypes "github.com/tendermint/abci/types"
	cmn "github.com/tendermint/tmlibs/common"
	tcmd "github.com/tendermint/tendermint/cmd/tendermint/commands"

	emtUtils "github.com/CyberMiles/travis/modules/vm/cmd/utils"
	abciApp "github.com/CyberMiles/travis/modules/vm/app"
	"github.com/CyberMiles/travis/modules/vm/ethereum"

	"github.com/CyberMiles/travis/app"
)

type Services struct {
	backend       *ethereum.Backend
	rpcClient     *rpc.Client
	emt           cmn.Service
	tmNode        *node.Node
}

func startServices(rootDir string, storeApp *app.StoreApp) (*Services, error) {

	// Step 1: Setup the go-ethereum node and start it
	emNode := emtUtils.MakeFullNode(context)
	startNode(context, emNode)

	// Setup the ABCI server and start it
	addr := context.GlobalString(emtUtils.ABCIAddrFlag.Name)
	abci := context.GlobalString(emtUtils.ABCIProtocolFlag.Name)

	// Fetch the registered service of this type
	var backend *ethereum.Backend
	if err := emNode.Service(&backend); err != nil {
		ethUtils.Fatalf("ethereum backend service not running: %v", err)
	}

	// In-proc RPC connection so ABCI.Query can be forwarded over the ethereum rpc
	rpcClient, err := emNode.Attach()
	if err != nil {
		ethUtils.Fatalf("Failed to attach to the inproc geth: %v", err)
	}

	// Create the ABCI app
	ethApp, err := abciApp.NewEthermintApplication(backend, rpcClient, nil)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	ethApp.SetLogger(emtUtils.EthermintLogger().With("module", "ethermint"))

	// Start the app on the ABCI server
	srv, err := server.NewServer(addr, abci, ethApp)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	srv.SetLogger(emtUtils.EthermintLogger().With("module", "abci-server"))

	if err := srv.Start(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Create Basecoin app
	basecoinApp, err := createBaseCoinApp(rootDir, storeApp)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	// Create & start tendermint node
	tmNode, err := startTendermint(basecoinApp)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	return &Services{backend, rpcClient, srv, tmNode}, nil
}

// startNode copies the logic from go-ethereum
func startNode(ctx *cli.Context, stack *ethereum.Node) {
	emtUtils.StartNode(stack)

	// Unlock any account specifically requested
	ks := stack.AccountManager().Backends(keystore.KeyStoreType)[0].(*keystore.KeyStore)

	passwords := ethUtils.MakePasswordList(ctx)
	unlocks := strings.Split(ctx.GlobalString(ethUtils.UnlockedAccountFlag.Name), ",")
	for i, account := range unlocks {
		if trimmed := strings.TrimSpace(account); trimmed != "" {
			unlockAccount(ctx, ks, trimmed, i, passwords)
		}
	}
	// Register wallet event handlers to open and auto-derive wallets
	events := make(chan accounts.WalletEvent, 16)
	stack.AccountManager().Subscribe(events)

	go func() {
		// Create an chain state reader for self-derivation
		rpcClient, err := stack.Attach()
		if err != nil {
			ethUtils.Fatalf("Failed to attach to self: %v", err)
		}
		stateReader := ethclient.NewClient(rpcClient)

		// Open and self derive any wallets already attached
		for _, wallet := range stack.AccountManager().Wallets() {
			if err := wallet.Open(""); err != nil {
				log.Warn("Failed to open wallet", "url", wallet.URL(), "err", err)
			} else {
				wallet.SelfDerive(accounts.DefaultBaseDerivationPath, stateReader)
			}
		}
		// Listen for wallet event till termination
		for event := range events {
			if event.Arrive {
				if err := event.Wallet.Open(""); err != nil {
					log.Warn("New wallet appeared, failed to open", "url",
						event.Wallet.URL(), "err", err)
				} else {
					log.Info("New wallet appeared", "url", event.Wallet.URL(),
						"status", event.Wallet.Status())
					event.Wallet.SelfDerive(accounts.DefaultBaseDerivationPath,
						stateReader)
				}
			} else {
				log.Info("Old wallet dropped", "url", event.Wallet.URL())
				event.Wallet.Close()
			}
		}
	}()
}

// tries unlocking the specified account a few times.
// nolint: unparam
func unlockAccount(ctx *cli.Context, ks *keystore.KeyStore, address string, i int,
	passwords []string) (accounts.Account, string) {

	account, err := ethUtils.MakeAddress(ks, address)
	if err != nil {
		ethUtils.Fatalf("Could not list accounts: %v", err)
	}
	for trials := 0; trials < 3; trials++ {
		prompt := fmt.Sprintf("Unlocking account %s | Attempt %d/%d", address, trials+1, 3)
		password := getPassPhrase(prompt, false, i, passwords)
		err = ks.Unlock(account, password)
		if err == nil {
			log.Info("Unlocked account", "address", account.Address.Hex())
			return account, password
		}
		if err, ok := err.(*keystore.AmbiguousAddrError); ok {
			log.Info("Unlocked account", "address", account.Address.Hex())
			return ambiguousAddrRecovery(ks, err, password), password
		}
		if err != keystore.ErrDecrypt {
			// No need to prompt again if the error is not decryption-related.
			break
		}
	}
	// All trials expended to unlock account, bail out
	ethUtils.Fatalf("Failed to unlock account %s (%v)", address, err)

	return accounts.Account{}, ""
}

// getPassPhrase retrieves the password associated with an account, either fetched
// from a list of preloaded passphrases, or requested interactively from the user.
// nolint: unparam
func getPassPhrase(prompt string, confirmation bool, i int, passwords []string) string {
	// If a list of passwords was supplied, retrieve from them
	if len(passwords) > 0 {
		if i < len(passwords) {
			return passwords[i]
		}
		return passwords[len(passwords)-1]
	}
	// Otherwise prompt the user for the password
	if prompt != "" {
		fmt.Println(prompt)
	}
	password, err := console.Stdin.PromptPassword("Passphrase: ")
	if err != nil {
		ethUtils.Fatalf("Failed to read passphrase: %v", err)
	}
	if confirmation {
		confirm, err := console.Stdin.PromptPassword("Repeat passphrase: ")
		if err != nil {
			ethUtils.Fatalf("Failed to read passphrase confirmation: %v", err)
		}
		if password != confirm {
			ethUtils.Fatalf("Passphrases do not match")
		}
	}
	return password
}

func ambiguousAddrRecovery(ks *keystore.KeyStore, err *keystore.AmbiguousAddrError,
	auth string) accounts.Account {

	fmt.Printf("Multiple key files exist for address %x:\n", err.Addr)
	for _, a := range err.Matches {
		fmt.Println("  ", a.URL)
	}
	fmt.Println("Testing your passphrase against all of them...")
	var match *accounts.Account
	for _, a := range err.Matches {
		if err := ks.Unlock(a, auth); err == nil {
			match = &a
			break
		}
	}
	if match == nil {
		ethUtils.Fatalf("None of the listed files could be unlocked.")
	}
	fmt.Printf("Your passphrase unlocked %s\n", match.URL)
	fmt.Println("In order to avoid this warning, remove the following duplicate key files:")
	for _, a := range err.Matches {
		if a != *match {
			fmt.Println("  ", a.URL)
		}
	}
	return *match
}

func startTendermint(basecoinApp abcitypes.Application) (*node.Node, error) {
	cfg, err := tcmd.ParseConfig()
	if err != nil {
		return nil, err
	}

	var papp proxy.ClientCreator
	if basecoinApp != nil {
		papp =proxy.NewLocalClientCreator(basecoinApp)
	} else {
		papp = proxy.DefaultClientCreator(cfg.ProxyApp, cfg.ABCI, cfg.DBDir())
	}

	// Create & start tendermint node
	n, err := node.NewNode(cfg,
		types.LoadOrGenPrivValidatorFS(cfg.PrivValidatorFile()),
		papp,
		node.DefaultGenesisDocProviderFunc(cfg),
		node.DefaultDBProvider,
		logger.With("module", "node"))
	if err != nil {
		return nil, err
	}

	err = n.Start()
	if err != nil {
		return nil, err
	}

	return n, nil
}
