package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	gnode "github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/hive/hivesim"
)

var (
	// the number of seconds before a sync is considered stalled or failed
	syncTimeout = 60 * time.Second
	sourceFiles = map[string]string{
		"genesis.json": "./chain/genesis.json",
		"chain.rlp":    "./chain/chain.rlp",
	}
	sinkFiles = map[string]string{
		"genesis.json": "./chain/genesis.json",
	}
)

func main() {
	// Load fork environment.
	var params hivesim.Params
	err := common.LoadJSON("chain/forkenv.json", &params)
	if err != nil {
		panic(err)
	}

	var suite = hivesim.Suite{
		Name: "sync",
		Description: `This suite of tests verifies that clients can sync from each other in different modes.
For each client, we test if it can serve as a sync source for all other clients (including itself).`,
	}
	suite.Add(hivesim.ClientTestSpec{
		Role:        "eth1",
		Name:        "CLIENT as sync source",
		Description: "This loads the test chain into the client and verifies whether it was imported correctly.",
		Parameters:  params,
		Files:       sourceFiles,
		Run: func(t *hivesim.T, c *hivesim.Client) {
			runSourceTest(t, c, params)
		},
	})
	hivesim.MustRunSuite(hivesim.New(), suite)
}

func runSourceTest(t *hivesim.T, c *hivesim.Client, params hivesim.Params) {
	// Check whether the source has imported its chain.rlp correctly.
	source := &node{c}
	if err := source.checkHead(); err != nil {
		t.Fatal(err)
	}

	// Configure sink to connect to the source node.
	enode, err := source.EnodeURL()
	if err != nil {
		t.Fatal("can't get node peer-to-peer endpoint:", enode)
	}
	sinkParams := params.Set("HIVE_BOOTNODE", enode)

	// Sync all sink nodes against the source.
	t.RunAllClients(hivesim.ClientTestSpec{
		Role:        "eth1",
		Name:        fmt.Sprintf("sync %s -> CLIENT", source.Type),
		Description: fmt.Sprintf("This test attempts to sync the chain from a %s node.", source.Type),
		Parameters:  sinkParams,
		Files:       sinkFiles,
		Run:         runSyncTest,
	})
}

func runSyncTest(t *hivesim.T, c *hivesim.Client) {
	node := &node{c}
	err := node.checkSync(t)
	if err != nil {
		t.Fatal("sync failed:", err)
	}
}

type node struct {
	*hivesim.Client
}

// checkSync waits for the node to reach the head of the chain.
func (n *node) checkSync(t *hivesim.T) error {
	var expectedHead types.Header
	err := common.LoadJSON("chain/headblock.json", &expectedHead)
	if err != nil {
		return fmt.Errorf("can't load expected header: %v", err)
	}
	wantHash := expectedHead.Hash()

	if err := n.triggerSync(t); err != nil {
		return err
	}

	var (
		timeout = time.After(syncTimeout)
		current = uint64(0)
	)
	for {
		select {
		case <-timeout:
			return fmt.Errorf("timeout (%v elapsed, current head is %d)", syncTimeout, current)
		default:
			block, err := n.head()
			if err != nil {
				t.Logf("error getting block from %s (%s): %v", n.Type, n.Container, err)
				return err
			}
			blockNumber := block.Number.Uint64()
			if blockNumber != current {
				t.Logf("%s has new head %d", n.Type, blockNumber)
			}
			if current == expectedHead.Number.Uint64() {
				if block.Hash() != wantHash {
					return fmt.Errorf("wrong head hash %x, want %x", block.Hash(), wantHash)
				}
				return nil // success
			}
			// check in a little while....
			current = blockNumber
			time.Sleep(1000 * time.Millisecond)
		}
	}
}

type rpcRequest struct {
	Method string
	Params []json.RawMessage
}

func (n *node) triggerSync(t *hivesim.T) error {
	// Load the engine requests generated by hivechain.
	var newpayload, fcu rpcRequest
	if err := common.LoadJSON("chain/headnewpayload.json", &newpayload); err != nil {
		return err
	}
	if err := common.LoadJSON("chain/headfcu.json", &fcu); err != nil {
		return err
	}

	// engine client setup
	token := [32]byte{0x73, 0x65, 0x63, 0x72, 0x65, 0x74, 0x73, 0x65, 0x63, 0x72, 0x65, 0x74, 0x73, 0x65, 0x63, 0x72, 0x65, 0x74, 0x73, 0x65, 0x63, 0x72, 0x65, 0x74, 0x73, 0x65, 0x63, 0x72, 0x65, 0x74, 0x73, 0x65}
	engineURL := fmt.Sprintf("http://%v:8551/", n.IP)
	ctx := context.Background()
	c, _ := rpc.DialOptions(ctx, engineURL, rpc.WithHTTPAuth(gnode.NewJWTAuth(token)))

	// deliver newPayload
	t.Logf("%s: %s", newpayload.Method, newpayload.Params)
	var npresp engine.PayloadStatusV1
	if err := c.Call(&npresp, newpayload.Method, conv2any(newpayload.Params)...); err != nil {
		return err
	}
	t.Logf("response: %+v", npresp)

	// deliver forkchoiceUpdated
	t.Logf("%s: %s", fcu.Method, fcu.Params)
	var fcuresp engine.ForkChoiceResponse
	if err := c.Call(&fcuresp, fcu.Method, conv2any(fcu.Params)...); err != nil {
		return err
	}
	t.Logf("response: %+v", fcuresp)
	return nil
}

// checkHead checks whether the remote chain head matches the given values.
func (n *node) checkHead() error {
	var expected types.Header
	err := common.LoadJSON("chain/headblock.json", &expected)
	if err != nil {
		return fmt.Errorf("can't load expected header: %v", err)
	}

	head, err := n.head()
	if err != nil {
		return fmt.Errorf("can't query chain head: %v", err)
	}
	if head.Hash() != expected.Hash() {
		return fmt.Errorf("wrong chain head %d (%s), want %d (%s)", head.Number, head.Hash().TerminalString(), expected.Number, expected.Hash().TerminalString())
	}
	return nil
}

// head returns the node's chain head.
func (n *node) head() (*types.Header, error) {
	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	return ethclient.NewClient(n.RPC()).HeaderByNumber(ctx, nil)
}

func conv2any[T any](s []T) []any {
	cpy := make([]any, len(s))
	for i := range s {
		cpy[i] = s[i]
	}
	return cpy
}
