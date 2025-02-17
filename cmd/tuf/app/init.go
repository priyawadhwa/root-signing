package app

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/peterbourgon/ff/v3/ffcli"
	pkeys "github.com/sigstore/root-signing/pkg/keys"
	prepo "github.com/sigstore/root-signing/pkg/repo"
	cjson "github.com/tent/canonical-json-go"
	"github.com/theupdateframework/go-tuf"
	"github.com/theupdateframework/go-tuf/data"
)

var threshold = 3

func Init() *ffcli.Command {
	var (
		flagset    = flag.NewFlagSet("tuf init", flag.ExitOnError)
		repository = flagset.String("repository", "", "path to initialize the staged repository")
		previous   = flagset.String("previous", "", "path to the previous repository")
		snapshot   = flagset.String("snapshot", "", "reference to an online snapshot signer")
		timestamp  = flagset.String("timestamp", "", "reference to an online timestamp signer")
		targets    = flagset.String("target-meta", "", "path to a target configuration file")
	)
	return &ffcli.Command{
		Name:       "init",
		ShortUsage: "tuf init initializes a new TUF repository",
		ShortHelp:  "tuf init initializes a new TUF repository",
		LongHelp: `tuf init initializes a new TUF repository to the 
		specified repository directory. It will create unpopulated directories 
		keys/, staged/, and staged/targets under the repository with threshold 3
		and a 4 month expiration.
		
	EXAMPLES
	# initialize repository at ceremony/YYYY-MM-DD
	tuf init -repository ceremony/YYYY-MM-DD`,
		FlagSet: flagset,
		Exec: func(ctx context.Context, args []string) error {
			if *repository == "" {
				return flag.ErrHelp
			}
			if *snapshot == "" {
				return flag.ErrHelp
			}
			if *timestamp == "" {
				return flag.ErrHelp
			}
			return InitCmd(ctx, *repository, *previous, *targets, *snapshot, *timestamp)
		},
	}
}

func InitCmd(ctx context.Context, directory, previous, targets, snapshotRef string, timestampRef string) error {
	// TODO: Validate directory is a good path.
	store := tuf.FileSystemStore(directory, nil)
	repo, err := tuf.NewRepoIndent(store, "", "\t", "sha512", "sha256")
	if err != nil {
		return err
	}
	curRootVersion, err := repo.RootVersion()
	if err != nil {
		return err
	}

	if previous == "" {
		// Only initialize if no previous specified.
		if err := repo.Init(false); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "TUF repository initialized at ", directory)
	}

	// Get the root.json file and initialize it with the expirations and thresholds
	expiration := time.Now().AddDate(0, 6, 0).UTC()

	// Add the keys we just provisioned to root and targets and revoke any removed ones.
	root, err := prepo.GetRootFromStore(store)
	if err != nil {
		return err
	}
	keys, err := getKeysFromDir(directory + "/keys")
	if err != nil {
		return err
	}
	var allRootKeys []*data.PublicKey
	for _, role := range []string{"root", "targets"} {
		currentKeyMap := map[string]bool{}
		for _, tufKey := range keys {
			currentKeyMap[tufKey.IDs()[0]] = true
			if err := repo.AddVerificationKeyWithExpiration(role, tufKey, expiration); err != nil {
				return err
			}
		}
		if role == "root" {
			// This retrieves all the new root keys, but before we revoke any.
			allRootKeys, err = repo.RootKeys()
			if err != nil {
				return err
			}
		}
		// Revoke any keys that we've rotated out
		oldKeys, ok := root.Roles[role]
		if ok {
			for _, oldKeyID := range oldKeys.KeyIDs {
				if _, ok := currentKeyMap[oldKeyID]; !ok {
					if err := repo.RevokeKey(role, oldKeyID); err != nil {
						return err
					}
				}
			}
		}
		if err := repo.SetThreshold(role, threshold); err != nil {
			return err
		}
	}

	// Revoke old root keys used for snapshot and timestamp and roles and add new keys.
	for role, keyRef := range map[string]string{"snapshot": snapshotRef, "timestamp": timestampRef} {
		signerKey, err := pkeys.GetKmsSigningKey(ctx, keyRef)
		if err != nil {
			return err
		}

		// Add key. The expiration will adjust in the snapshot/timestamp step.
		if err := repo.AddVerificationKeyWithExpiration(role, signerKey.Key, data.DefaultExpires(role)); err != nil {
			return err
		}

		if err := repo.SetThreshold(role, 1); err != nil {
			return err
		}
	}

	// Add targets (copy them into the repository and add them to the targets.json)
	targetCfg, err := os.ReadFile(targets)
	if err != nil {
		return err
	}
	meta, err := prepo.TargetMetaFromString(targetCfg)
	if err != nil {
		return err
	}

	for tt, custom := range meta {
		from, err := os.Open(tt)
		if err != nil {
			return err
		}
		defer from.Close()
		base := filepath.Base(tt)
		to, err := os.Create(filepath.Join(directory, "staged", "targets", base))
		if err != nil {
			return err
		}
		defer to.Close()
		if _, err := io.Copy(to, from); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Created target file at ", to.Name())
		if err := repo.AddTargetWithExpiresToPreferredRole(base, custom, expiration, "targets"); err != nil {
			return fmt.Errorf("error adding targets %w", err)
		}
	}

	if err := repo.SetThreshold("targets", threshold); err != nil {
		return err
	}

	// Add blank signatures to root and targets
	t, err := prepo.GetTargetsFromStore(store)
	if err != nil {
		return err
	}
	if err := setMetaWithSigKeyIDs(store, "targets.json", t, allRootKeys); err != nil {
		return err
	}

	// We manually increment the root version in case adding the verification keys did not
	// increment the root version (because of no change).
	root, err = prepo.GetRootFromStore(store)
	if err != nil {
		return err
	}
	root.Version = curRootVersion + 1
	root.Expires = expiration
	return setMetaWithSigKeyIDs(store, "root.json", root, keys)
}

func setSignedMeta(store tuf.LocalStore, role string, s *data.Signed) error {
	b, err := jsonMarshal(s)
	if err != nil {
		return err
	}
	return store.SetMeta(role, b)
}

func setMetaWithSigKeyIDs(store tuf.LocalStore, role string, meta interface{}, keys []*data.PublicKey) error {
	signed, err := jsonMarshal(meta)
	if err != nil {
		return err
	}

	// Add empty sigs
	emptySigs := make([]data.Signature, 0, 1)

	for _, key := range keys {
		for _, id := range key.IDs() {
			emptySigs = append(emptySigs, data.Signature{
				KeyID: id,
			})
		}
	}

	return setSignedMeta(store, role, &data.Signed{Signatures: emptySigs, Signed: signed})
}

func jsonMarshal(v interface{}) ([]byte, error) {
	b, err := cjson.Marshal(v)
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	if err := json.Indent(&out, b, "", "\t"); err != nil {
		return nil, err
	}

	return out.Bytes(), nil
}

func getKeysFromDir(dir string) ([]*data.PublicKey, error) {
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tufKeys []*data.PublicKey
	for _, file := range files {
		if file.IsDir() {
			key, err := pkeys.SigningKeyFromDir(filepath.Join(dir, file.Name()))
			if err != nil {
				return nil, err
			}
			tufKey, err := pkeys.ToTufKey(*key)
			if err != nil {
				return nil, err
			}
			tufKeys = append(tufKeys, tufKey)
		}
	}
	return tufKeys, err
}
