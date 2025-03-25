package internal

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	_ "embed"

	gethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	ecrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/hashicorp/go-uuid"
	"github.com/prysmaticlabs/prysm/v5/config/params"
	"github.com/prysmaticlabs/prysm/v5/crypto/bls/common"
	"github.com/prysmaticlabs/prysm/v5/runtime/interop"
	"github.com/prysmaticlabs/prysm/v5/runtime/version"
	keystorev4 "github.com/wealdtech/go-eth2-wallet-encryptor-keystorev4"
	"gopkg.in/yaml.v2"
)

var (
	defaultDiscoveryPrivKey = "a11ac89899cd86e36b6fb881ec1255b8a92a688790b7d950f8b7d8dd626671fb"
	defaultDiscoveryEnodeID = "3479db4d9217fb5d7a8ed4d61ac36e120b05d36c2eefb795dc42ff2e971f251a2315f5649ea1833271e020b9adc98d5db9973c7ed92d6b2f1f2223088c3d852f"
)

// minimumGenesisDelay is the minimum delay for the genesis time. This is required
// because lighthouse takes some time to start and we need to make sure it is ready
// otherwise, some blocks are missed.
var MinimumGenesisDelay uint64 = 10

//go:embed utils/rollup.json
var opRollupConfig []byte

//go:embed utils/genesis.json
var opGenesis []byte

//go:embed utils/state.json
var opState []byte

//go:embed config.yaml.tmpl
var clConfigContent []byte

type ArtifactsBuilder struct {
	outputDir         string
	applyLatestL1Fork bool
	genesisDelay      uint64
}

func NewArtifactsBuilder() *ArtifactsBuilder {
	return &ArtifactsBuilder{
		outputDir:         "",
		applyLatestL1Fork: false,
		genesisDelay:      MinimumGenesisDelay,
	}
}

func (b *ArtifactsBuilder) OutputDir(outputDir string) *ArtifactsBuilder {
	b.outputDir = outputDir
	return b
}

func (b *ArtifactsBuilder) ApplyLatestL1Fork(applyLatestL1Fork bool) *ArtifactsBuilder {
	b.applyLatestL1Fork = applyLatestL1Fork
	return b
}

func (b *ArtifactsBuilder) GenesisDelay(genesisDelaySeconds uint64) *ArtifactsBuilder {
	b.genesisDelay = genesisDelaySeconds
	return b
}

type Artifacts struct {
	Out *output
}

func (b *ArtifactsBuilder) Build() (*Artifacts, error) {
	homeDir, err := GetHomeDir()
	if err != nil {
		return nil, err
	}
	if b.outputDir == "" {
		// Use the $HOMEDIR/devnet as the default output
		b.outputDir = filepath.Join(homeDir, "devnet")
	}

	out := &output{dst: b.outputDir, homeDir: homeDir}

	// check if the output directory exists
	if out.Exists("") {
		log.Printf("deleting existing output directory %s", b.outputDir)
		if err := out.Remove(""); err != nil {
			return nil, err
		}
	}

	if b.genesisDelay < MinimumGenesisDelay {
		log.Printf("genesis delay must be at least %d seconds, using %d", MinimumGenesisDelay, MinimumGenesisDelay)
		b.genesisDelay = MinimumGenesisDelay
	}

	// enable the latest fork in config.yaml or not
	var latestForkEpoch string
	if b.applyLatestL1Fork {
		latestForkEpoch = "0"
	} else {
		latestForkEpoch = "18446744073709551615"
	}
	clConfigContentStr := strings.Replace(string(clConfigContent), "{{.LatestForkEpoch}}", latestForkEpoch, 1)

	// load the config.yaml file
	clConfig, err := params.UnmarshalConfig([]byte(clConfigContentStr), nil)
	if err != nil {
		return nil, err
	}
	if err := params.SetActive(clConfig); err != nil {
		return nil, err
	}

	genesisTime := uint64(time.Now().Add(time.Duration(b.genesisDelay) * time.Second).Unix())
	config := params.BeaconConfig()

	gen := interop.GethTestnetGenesis(genesisTime, config)
	// HACK: fix this in prysm?
	gen.Config.DepositContractAddress = gethcommon.HexToAddress(config.DepositContractAddress)

	// add pre-funded accounts
	prefundedBalance, _ := new(big.Int).SetString("10000000000000000000000", 16)

	for _, privStr := range prefundedAccounts {
		priv, err := getPrivKey(privStr)
		if err != nil {
			return nil, err
		}
		addr := ecrypto.PubkeyToAddress(priv.PublicKey)
		gen.Alloc[addr] = types.Account{
			Balance: prefundedBalance,
			Nonce:   1,
		}
	}

	// Apply Optimism pre-state
	{
		var state struct {
			L1StateDump string `json:"l1StateDump"`
		}
		if err := json.Unmarshal(opState, &state); err != nil {
			return nil, fmt.Errorf("failed to unmarshal opState: %w", err)
		}

		decoded, err := base64.StdEncoding.DecodeString(state.L1StateDump)
		if err != nil {
			return nil, fmt.Errorf("failed to decode opState: %w", err)
		}

		// Create gzip reader from the base64 decoded data
		gr, err := gzip.NewReader(bytes.NewReader(decoded))
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer gr.Close()

		// Read and decode the contents
		contents, err := io.ReadAll(gr)
		if err != nil {
			return nil, fmt.Errorf("failed to read opState: %w", err)
		}

		var alloc types.GenesisAlloc
		if err := json.Unmarshal(contents, &alloc); err != nil {
			return nil, fmt.Errorf("failed to unmarshal opState: %w", err)
		}
		for addr, account := range alloc {
			gen.Alloc[addr] = account
		}
	}

	block := gen.ToBlock()
	log.Printf("Genesis block hash: %s", block.Hash())

	var v int
	if b.applyLatestL1Fork {
		v = version.Electra
	} else {
		v = version.Deneb
	}

	priv, pub, err := interop.DeterministicallyGenerateKeys(0, 100)
	if err != nil {
		return nil, err
	}

	depositData, roots, err := interop.DepositDataFromKeysWithExecCreds(priv, pub, 100)
	if err != nil {
		return nil, err
	}

	opts := make([]interop.PremineGenesisOpt, 0)
	opts = append(opts, interop.WithDepositData(depositData, roots))

	state, err := interop.NewPreminedGenesis(context.Background(), genesisTime, 0, 100, v, block, opts...)
	if err != nil {
		return nil, err
	}

	err = out.WriteBatch(map[string]interface{}{
		"testnet/config.yaml":                 func() ([]byte, error) { return convert(config) },
		"testnet/genesis.ssz":                 state,
		"genesis.json":                        gen,
		"jwtsecret":                           defaultJWTToken,
		"testnet/boot_enr.yaml":               "[]",
		"testnet/deploy_block.txt":            "0",
		"testnet/deposit_contract_block.txt":  "0",
		"testnet/genesis_validators_root.txt": hex.EncodeToString(state.GenesisValidatorsRoot()),
		"data_validator/":                     &lighthouseKeystore{privKeys: priv},
		"deterministic_p2p_key.txt":           defaultDiscoveryPrivKey,
	})
	if err != nil {
		return nil, err
	}

	{
		opTimestamp := genesisTime + 2

		// override l2 genesis, make the timestamp start 2 seconds after the L1 genesis
		newOpGenesis, err := overrideJSON(opGenesis, map[string]interface{}{
			"timestamp": hexutil.Uint64(opTimestamp).String(),
		})
		if err != nil {
			return nil, err
		}

		// the hash of the genesis has changed beause of the timestamp so we need to account for that
		var opGenesisObj core.Genesis
		if err := json.Unmarshal(newOpGenesis, &opGenesisObj); err != nil {
			return nil, fmt.Errorf("failed to unmarshal opGenesis: %w", err)
		}

		opGenesisHash := opGenesisObj.ToBlock().Hash()

		// override rollup.json with the real values for the L1 chain and the correct timestamp
		newOpRollup, err := overrideJSON(opRollupConfig, map[string]interface{}{
			"genesis": map[string]interface{}{
				"l2_time": opTimestamp, // this one not in hex
				"l1": map[string]interface{}{
					"hash":   block.Hash().String(),
					"number": 0,
				},
				"l2": map[string]interface{}{
					"hash":   opGenesisHash.String(),
					"number": 0,
				},
			},
			"chain_op_config": map[string]interface{}{ // TODO: Read this from somewhere (genesis??)
				"eip1559Elasticity":        6,
				"eip1559Denominator":       50,
				"eip1559DenominatorCanyon": 250,
			},
		})
		if err != nil {
			return nil, err
		}

		if err := out.WriteFile("l2-genesis.json", newOpGenesis); err != nil {
			return nil, err
		}
		if err := out.WriteFile("rollup.json", newOpRollup); err != nil {
			return nil, err
		}
	}

	return &Artifacts{Out: out}, nil
}

func overrideJSON(jsonData []byte, overrides map[string]interface{}) ([]byte, error) {
	// Parse original JSON into a map
	var original map[string]interface{}
	if err := json.Unmarshal(jsonData, &original); err != nil {
		return nil, fmt.Errorf("failed to unmarshal original JSON: %w", err)
	}

	// Recursively merge the overrides into the original
	mergeMap(original, overrides)

	// Marshal back to JSON
	result, err := json.Marshal(original)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal modified JSON: %w", err)
	}

	return result, nil
}

// mergeMap recursively merges src into dst
func mergeMap(dst, src map[string]interface{}) {
	for key, srcVal := range src {
		if dstVal, exists := dst[key]; exists {
			// If both values are maps, merge them recursively
			if dstMap, ok := dstVal.(map[string]interface{}); ok {
				if srcMap, ok := srcVal.(map[string]interface{}); ok {
					mergeMap(dstMap, srcMap)
					continue
				}
			}
		}
		// For all other cases, override the value
		dst[key] = srcVal
	}
}

func getPrivKey(privStr string) (*ecdsa.PrivateKey, error) {
	privBuf, err := hex.DecodeString(strings.TrimPrefix(privStr, "0x"))
	if err != nil {
		return nil, err
	}

	priv, err := ecrypto.ToECDSA(privBuf)
	if err != nil {
		return nil, err
	}
	return priv, nil
}

func Connect(service, port string) string {
	return fmt.Sprintf(`{{Service "%s" "%s"}}`, service, port)
}

type output struct {
	dst string

	homeDir string
	lock    sync.Mutex
}

func (o *output) AbsoluteDstPath() (string, error) {
	return filepath.Abs(o.dst)
}

func (o *output) Exists(path string) bool {
	_, err := os.Stat(filepath.Join(o.dst))
	return err == nil
}

func (o *output) Remove(path string) error {
	return os.RemoveAll(filepath.Join(o.dst, path))
}

func (o *output) CopyFile(src string, dst string) error {
	// Open the source file
	sourceFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer sourceFile.Close()

	// Create the destination directory if it doesn't exist
	dstPath := filepath.Join(o.dst, dst)
	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Create the destination file
	destFile, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("failed to create destination file: %w", err)
	}
	defer destFile.Close()

	// Copy the contents
	if _, err := io.Copy(destFile, sourceFile); err != nil {
		return fmt.Errorf("failed to copy file contents: %w", err)
	}

	// Copy file permissions from source to destination
	sourceInfo, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("failed to get source file info: %w", err)
	}

	if err := os.Chmod(dstPath, sourceInfo.Mode()); err != nil {
		return fmt.Errorf("failed to set destination file permissions: %w", err)
	}

	return nil
}

func (o *output) WriteBatch(data map[string]interface{}) error {
	for dst, data := range data {
		if err := o.WriteFile(dst, data); err != nil {
			return err
		}
	}
	return nil
}

func (o *output) LogOutput(name string) (*os.File, error) {
	// lock this because some services might be trying to access this in parallel
	o.lock.Lock()
	defer o.lock.Unlock()

	path := filepath.Join(o.dst, "logs", name+".log")

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	logOutput, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return logOutput, nil
}

func (o *output) WriteFile(dst string, data interface{}) error {
	dst = filepath.Join(o.dst, dst)

	var dataRaw []byte
	var err error

	if raw, ok := data.([]byte); ok {
		dataRaw = raw
	} else if raw, ok := data.(string); ok {
		dataRaw = []byte(raw)
	} else if ssz, ok := data.(sszObject); ok {
		if dataRaw, err = ssz.MarshalSSZ(); err != nil {
			return err
		}
	} else if encObj, ok := data.(encObject); ok {
		// create a new output for this sub-object and delegate the full encoding to it
		if err = encObj.Encode(&output{dst: dst}); err != nil {
			return err
		}
		return nil
	} else if encFn, ok := data.(func() ([]byte, error)); ok {
		if dataRaw, err = encFn(); err != nil {
			return err
		}
	} else {
		// figure out how to decode the object given the file extension
		ext := filepath.Ext(dst)
		if ext == ".json" {
			if dataRaw, err = json.MarshalIndent(data, "", "\t"); err != nil {
				return err
			}
		} else if ext == ".yaml" {
			if dataRaw, err = yaml.Marshal(data); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("unsupported file extension: %s", ext)
		}
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, dataRaw, 0644); err != nil {
		return err
	}
	return nil
}

var secret = "secret"

type lighthouseKeystore struct {
	privKeys []common.SecretKey
}

func (l *lighthouseKeystore) Encode(o *output) error {
	for _, key := range l.privKeys {
		encryptor := keystorev4.New()
		cryptoFields, err := encryptor.Encrypt(key.Marshal(), secret)
		if err != nil {
			return err
		}

		id, _ := uuid.GenerateUUID()

		pubKeyHex := "0x" + hex.EncodeToString(key.PublicKey().Marshal())
		item := map[string]interface{}{
			"crypto":      cryptoFields,
			"uuid":        id,
			"pubkey":      pubKeyHex[2:], // without 0x in the json file
			"version":     4,
			"description": "",
		}
		valJSON, err := json.MarshalIndent(item, "", "\t")
		if err != nil {
			return err
		}

		if err := o.WriteBatch(map[string]interface{}{
			"validators/" + pubKeyHex + "/voting-keystore.json": valJSON,
			"secrets/" + pubKeyHex:                              secret,
		}); err != nil {
			return err
		}
	}

	return nil
}

type encObject interface {
	Encode(o *output) error
}

type sszObject interface {
	MarshalSSZ() ([]byte, error)
}

func GetHomeDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("error getting user home directory: %w", err)
	}

	// Define the path for our custom home directory
	customHomeDir := filepath.Join(homeDir, ".playground")

	// Create output directory if it doesn't exist
	if err := os.MkdirAll(customHomeDir, 0755); err != nil {
		return "", fmt.Errorf("error creating output directory: %v", err)
	}

	return customHomeDir, nil
}

func convert(config *params.BeaconChainConfig) ([]byte, error) {
	val := reflect.ValueOf(config).Elem()

	vals := []string{}
	for i := 0; i < val.NumField(); i++ {
		// only encode the public fields with tag 'yaml'
		tag := val.Type().Field(i).Tag.Get("yaml")
		if tag == "" {
			continue
		}

		// decode the type of the value
		typ := val.Field(i).Type()

		var resTyp string
		if isByteArray(typ) || isByteSlice(typ) {
			resTyp = "0x" + hex.EncodeToString(val.Field(i).Bytes())
		} else {
			// basic types
			switch typ.Kind() {
			case reflect.String:
				resTyp = val.Field(i).String()
			case reflect.Uint8, reflect.Uint64:
				resTyp = fmt.Sprintf("%d", val.Field(i).Uint())
			case reflect.Int:
				resTyp = fmt.Sprintf("%d", val.Field(i).Int())
			default:
				panic(fmt.Sprintf("BUG: unsupported type, tag '%s', err: '%s'", tag, val.Field(i).Kind()))
			}
		}

		vals = append(vals, fmt.Sprintf("%s: %s", tag, resTyp))
	}

	return []byte(strings.Join(vals, "\n")), nil
}

func isByteArray(t reflect.Type) bool {
	return t.Kind() == reflect.Array && t.Elem().Kind() == reflect.Uint8
}

func isByteSlice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8
}

var prefundedAccounts = []string{
	"0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80",
	"0x59c6995e998f97a5a0044966f0945389dc9e86dae88c7a8412f4603b6b78690d",
	"0x5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a",
	"0x7c852118294e51e653712a81e05800f419141751be58f605c371e15141b007a6",
	"0x47e179ec197488593b187f80a00eb0da91f1b9d0b13f8733639f19c30a34926a",
	"0x8b3a350cf5c34c9194ca85829a2df0ec3153be0318b5e2d3348e872092edffba",
	"0x92db14e403b83dfe3df233f83dfa3a0d7096f21ca9b0d6d6b8d88b2b4ec1564e",
	"0x4bbbf85ce3377467afe5d46f804f221813b2bb87f24d81f60f1fcdbf7cbf4356",
	"0xdbda1821b80551c9d65939329250298aa3472ba22feea921c0cf5d620ea67b97",
	"0x2a871d0798f97d79848a013d4936a73bf4cc922c825d33c1cf7073dff6d409c6",
}
