//go:build linux && !ppc64le

/***************************************************************
*
* Copyright (C) 2024, Pelican Project, Morgridge Institute for Research
*
* Licensed under the Apache License, Version 2.0 (the "License"); you
* may not use this file except in compliance with the License.  You may
* obtain a copy of the License at
*
*    http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
***************************************************************/

package lotman

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/pelicanplatform/pelican/param"
)

var (
	// A mutex for the Lotman caller context -- make sure we're calling lotman functions with the appropriate caller
	callerMutex = sync.RWMutex{}

	// Global vars used for one-time Lotman lib initialization
	lotmanInitTried   = false
	lotmanInitSuccess = false

	initializedLots []Lot

	// Lotman func signatures we'll bind to the underlying C headers
	LotmanVersion func() string
	// Strings in go are immutable, so they're actually passed to the underlying SO as `const`. To get dynamic
	// output, we need to pass a pointer to a byte array
	LotmanAddLot              func(lotJSON string, errMsg *[]byte) int32
	LotmanGetLotJSON          func(lotName string, recursive bool, output *[]byte, errMsg *[]byte) int32
	LotmanAddToLot            func(additionsJSON string, errMsg *[]byte) int32
	LotmanUpdateLot           func(updateJSON string, errMsg *[]byte) int32
	LotmanDeleteLotsRecursive func(lotName string, errMsg *[]byte) int32

	// Auxilliary functions
	LotmanLotExists     func(lotName string, errMsg *[]byte) int32
	LotmanSetContextStr func(contextKey string, contextValue string, errMsg *[]byte) int32
	// Functions that would normally take a char *** as an argument take an *unsafe.Pointer instead because
	// these functions are responsible for allocating and deallocating the memory for the char ***. The Go
	// runtime will handle the memory management for the *unsafe.Pointer.
	LotmanGetLotOwners func(lotName string, recursive bool, output *unsafe.Pointer, errMsg *[]byte) int32
	// Here, getSelf means get the lot proper if it's a self parent
	LotmanGetLotParents  func(lotName string, recursive bool, getSelf bool, output *unsafe.Pointer, errMsg *[]byte) int32
	LotmanGetLotsFromDir func(dir string, recursive bool, output *unsafe.Pointer, errMsg *[]byte) int32
)

type (
	Int64FromFloat struct {
		Value int64
	}

	LotPaths struct {
		Path      string `json:"path" mapstructure:"Path"`
		Recursive bool   `json:"recursive" mapstructure:"Recursive"`
		LotName   string `json:"lot_name,omitempty"`
	}

	LotValueMapInt struct {
		LotName string         `json:"lot_name"`
		Value   Int64FromFloat `json:"value"`
	}

	LotValueMapFloat struct {
		LotName string  `json:"lot_name"`
		Value   float64 `json:"value"`
	}

	MPA struct {
		DedicatedGB     *float64        `json:"dedicated_GB,omitempty" mapstructure:"DedicatedGB"`
		OpportunisticGB *float64        `json:"opportunistic_GB,omitempty" mapstructure:"OpportunisticGB"`
		MaxNumObjects   *Int64FromFloat `json:"max_num_objects,omitempty" mapstructure:"MaxNumObjects"`
		CreationTime    *Int64FromFloat `json:"creation_time,omitempty" mapstructure:"CreationTime"`
		ExpirationTime  *Int64FromFloat `json:"expiration_time,omitempty" mapstructure:"ExpirationTime"`
		DeletionTime    *Int64FromFloat `json:"deletion_time,omitempty" mapstructure:"DeletionTime"`
	}

	RestrictiveMPA struct {
		DedicatedGB     LotValueMapFloat `json:"dedicated_GB"`
		OpportunisticGB LotValueMapFloat `json:"opportunistic_GB"`
		MaxNumObjects   LotValueMapInt   `json:"max_num_objects"`
		CreationTime    LotValueMapInt   `json:"creation_time"`
		ExpirationTime  LotValueMapInt   `json:"expiration_time"`
		DeletionTime    LotValueMapInt   `json:"deletion_time"`
	}

	UsageMapFloat struct {
		SelfContrib     float64 `json:"self_contrib,omitempty"`
		ChildrenContrib float64 `json:"children_contrib,omitempty"`
		Total           float64 `json:"total"`
	}

	UsageMapInt struct {
		SelfContrib     Int64FromFloat `json:"self_contrib,omitempty"`
		ChildrenContrib Int64FromFloat `json:"children_contrib,omitempty"`
		Total           Int64FromFloat `json:"total"`
	}

	LotUsage struct {
		GBBeingWritten      UsageMapFloat `json:"GB_being_written,omitempty"`
		ObjectsBeingWritten UsageMapInt   `json:"objects_being_written,omitempty"`
		DedicatedGB         UsageMapFloat `json:"dedicated_GB,omitempty"`
		OpportunisticGB     UsageMapFloat `json:"opportunistic_GB,omitempty"`
		NumObjects          UsageMapInt   `json:"num_objects,omitempty"`
		TotalGB             UsageMapFloat `json:"total_GB,omitempty"`
	}

	Lot struct {
		LotName string `json:"lot_name" mapstructure:"LotName"`
		Owner   string `json:"owner,omitempty" mapstructure:"Owner"`
		// We don't expose Owners via map structure because that's not something we can configure. It's a derived value
		Owners         []string        `json:"owners,omitempty"`
		Parents        []string        `json:"parents" mapstructure:"Parents"`
		// While we _could_ expose Children, that complicates things so for now we keep it hidden from the config
		Children       *[]string       `json:"children,omitempty"`
		Paths          []LotPaths      `json:"paths,omitempty" mapstructure:"Paths"`
		MPA            *MPA            `json:"management_policy_attrs,omitempty" mapstructure:"ManagementPolicyAttrs"`
		// Again, these are derived
		RestrictiveMPA *RestrictiveMPA `json:"restrictive_management_policy_attrs,omitempty"`
		Usage          *LotUsage       `json:"usage,omitempty"`
	}

	ParentUpdate struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}

	PathUpdate struct {
		Current   string `json:"current"`
		New       string `json:"new"`
		Recursive bool   `json:"recursive"`
	}

	LotUpdate struct {
		LotName string          `json:"lot_name"`
		Owner   *string         `json:"owner,omitempty"`
		Parents *[]ParentUpdate `json:"parents,omitempty"`
		Paths   *[]PathUpdate   `json:"paths,omitempty"`
		MPA     *MPA            `json:"management_policy_attrs,omitempty"`
	}
)

// Lotman has a tendency to return an int as 123.0 instead of 123. This struct is used to unmarshal
// those values into an int64
func (i *Int64FromFloat) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	i.Value = int64(f)
	return nil
}

func (i Int64FromFloat) MarshalJSON() ([]byte, error) {
	return json.Marshal(i.Value)
}

func cArrToGoArr(cArr *unsafe.Pointer) []string {
	ptr := uintptr(*cArr)
	var goArr []string
	for {
		// Read the uintptr at the current position.
		strPtr := *(*uintptr)(unsafe.Pointer(ptr))

		// Break if the uintptr is null.
		if strPtr == 0 {
			break
		}

		// Create a Go string from the null-terminated string.
		goStr := ""
		for i := 0; ; i++ {
			// Read the byte at the current position.
			b := *(*byte)(unsafe.Pointer(strPtr + uintptr(i)))

			// Break if the byte is null.
			if b == 0 {
				break
			}

			// Append the byte to the Go string.
			goStr += string(b)
		}

		// Append the Go string to the slice.
		goArr = append(goArr, goStr)

		// Move to the next uintptr.
		ptr += unsafe.Sizeof(uintptr(0))
	}

	return goArr
}

// Trim any buffer we get back from LotMan to the first null char
func trimBuf(buf *[]byte) {
	// Find the index of the first null character
	nullIndex := bytes.IndexByte(*buf, 0)

	// Trim the slice after the first null character
	if nullIndex != -1 {
		*buf = (*buf)[:nullIndex]
	}
}

// Use the detected runtime to predict the location of the LotMan library.
func getLotmanLib() string {
	fallbackPaths := []string{
		"/usr/lib64/libLotMan.so",
		"/usr/local/lib64/libLotMan.so",
		"/opt/local/lib64/libLotMan.so",
	}

	switch runtime.GOOS {
	case "linux":
		configuredPath := param.Lotman_LibLocation.GetString()
		if configuredPath != "" {
			if _, err := os.Stat(configuredPath); err == nil {
				return configuredPath
			}
			log.Errorln("libLotMan.so not found in configured path, attempting to find using known fallbacks")
		}

		for _, path := range fallbackPaths {
			if _, err := os.Stat(path); err == nil {
				return path
			}
		}
		panic("libLotMan.so not found in any of the known paths")
	default:
		panic(fmt.Errorf("GOOS=%s is not supported", runtime.GOOS))
	}
}

func GetAuthorizedCallers(lotName string) (*[]string, error) {
	// A caller is authorized if they own a parent of the lot. In the case of self-parenting lots, the owner is authorized.
	errMsg := make([]byte, 2048)
	cParents := unsafe.Pointer(nil)

	// Get immediate parents (including self to determine rootliness). We'll use them to determine owners
	// who are allowed to manipulate, and thus delete, the lot
	ret := LotmanGetLotParents(lotName, false, true, &cParents, &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return nil, errors.Errorf("Failed to determine %s's parents: %s", lotName, string(errMsg))
	}

	parents := cArrToGoArr(&cParents)

	owners := []string{}
	internalOwners := []string{}
	for _, parent := range parents {
		cOwners := unsafe.Pointer(nil)
		LotmanGetLotOwners(parent, true, &cOwners, &errMsg)
		if ret != 0 {
			trimBuf(&errMsg)
			return nil, errors.Errorf("Failed to determine appropriate owners of %s's parents: %s", lotName, string(errMsg))
		}

		internalOwners = append(internalOwners, cArrToGoArr(&cOwners)...)
	}

	// Deduplicate the owners
	occurred := map[string]bool{}
	for e := range internalOwners {
		if !occurred[internalOwners[e]] {
			occurred[internalOwners[e]] = true
			owners = append(owners, internalOwners[e])
		}
	}

	return &owners, nil
}

// Initialize the LotMan library and bind its functions to the global vars
// We also perform a bit of extra setup such as setting the lotman db location
func InitLotman() bool {
	// If we've already tried to init the library, return the result of that attempt
	if lotmanInitTried {
		return lotmanInitSuccess
	}
	log.Infof("Initializing LotMan...")
	lotmanInitTried = true

	// dlopen the LotMan library
	lotmanLib, err := purego.Dlopen(getLotmanLib(), purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		log.Errorf("Error opening LotMan library: %v", err)
		return false
	}

	// Register LotMan funcs
	purego.RegisterLibFunc(&LotmanVersion, lotmanLib, "lotman_version")
	// C
	purego.RegisterLibFunc(&LotmanAddLot, lotmanLib, "lotman_add_lot")
	// R
	purego.RegisterLibFunc(&LotmanGetLotJSON, lotmanLib, "lotman_get_lot_as_json")
	// U
	purego.RegisterLibFunc(&LotmanUpdateLot, lotmanLib, "lotman_update_lot")
	// D
	purego.RegisterLibFunc(&LotmanDeleteLotsRecursive, lotmanLib, "lotman_remove_lots_recursive")

	// Auxilliary functions
	purego.RegisterLibFunc(&LotmanLotExists, lotmanLib, "lotman_lot_exists")
	purego.RegisterLibFunc(&LotmanSetContextStr, lotmanLib, "lotman_set_context_str")
	purego.RegisterLibFunc(&LotmanGetLotOwners, lotmanLib, "lotman_get_owners")
	purego.RegisterLibFunc(&LotmanGetLotParents, lotmanLib, "lotman_get_parent_names")
	purego.RegisterLibFunc(&LotmanGetLotsFromDir, lotmanLib, "lotman_get_lots_from_dir")

	// Set the lot_home context -- where the db lives
	lotHome := param.Lotman_DbLocation.GetString()

	errMsg := make([]byte, 2048)

	log.Infof("Setting lot_home context to %s", lotHome)
	ret := LotmanSetContextStr("lot_home", lotHome, &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		log.Errorf("Error setting lot_home context: %s", string(errMsg))
		return false
	}

	defaultInitialized := false
	rootInitialized := false

	err = param.Lotman_Lots.Unmarshal(&initializedLots)
	if err != nil {
		log.Warningf("Error while unmarshaling Lots from config: %v", err)
	}

	federationIssuer := param.Federation_DiscoveryUrl.GetString()
	if federationIssuer == "" {
		federationIssuer = param.Federation_DirectorUrl.GetString()
	}

	// Create the basic lots if they don't already exist. We'll make one for default
	// and one for the root namespace
	ret = LotmanLotExists("default", &errMsg)
	if ret < 0 {
		trimBuf(&errMsg)
		log.Errorf("Error checking if default lot exists: %s", string(errMsg))
		return false
	} else if ret == 0 {
		// First we try to create the lots that might be configured via Pelican.yaml. If there are none, we'll use
		// a few default values
		for _, lot := range initializedLots {
			if lot.LotName == "default" {
				err := CreateLot(&lot, federationIssuer)
				if err != nil {
					log.Errorf("Error creating default lot: %v", err)
					return false
				}
				defaultInitialized = true
			}
		}

		if !defaultInitialized {
			// Create the default lot

			// Under our model, we set owner to the issuer. Since this is owned by the federation, we set it in order of preference:
			// 1. The federation's discovery url
			// 2. The federation's director url
			// TODO: Consider what happens to the lot if either of these values change in the future after the lot is created?

			if federationIssuer == "" {
				log.Errorf("no federation issuer set in config")
				return false
			}

			initDedicatedGB := float64(0)
			initOpportunisticGB := float64(0)
			defaultLot := Lot{
				LotName: "default",
				// Set the owner to the Federation's discovery url -- under this model, we can treat it like an issuer
				Owner: federationIssuer,
				// A self-parent lot indicates superuser status
				Parents: []string{"default"},
				MPA: &MPA{
					DedicatedGB:     &initDedicatedGB,
					OpportunisticGB: &initOpportunisticGB,
					MaxNumObjects:   &Int64FromFloat{Value: 0},
					CreationTime:    &Int64FromFloat{Value: 0},
					ExpirationTime:  &Int64FromFloat{Value: 0},
					DeletionTime:    &Int64FromFloat{Value: 0},
				},
			}

			log.Debugf("Creating the default lot defined by %v", defaultLot)
			err = CreateLot(&defaultLot, federationIssuer)
			if err != nil {
				log.Errorf("Error creating default lot: %v", err)
				return false
			}
		}

		log.Infof("Created default lot")
	}

	ret = LotmanLotExists("root", &errMsg)
	if ret < 0 {
		trimBuf(&errMsg)
		log.Errorf("Error checking if root lot exists: %s", string(errMsg))
		return false
	} else if ret == 0 {
		// Try to create the root lot based on what we have in the config
		for _, lot := range initializedLots {
			if lot.LotName == "root" {
				err := CreateLot(&lot, federationIssuer)
				if err != nil {
					log.Errorf("error creating root lot: %v", err)
					return false
				}
				rootInitialized = true
			}
		}

		if !rootInitialized {
			// Create the root lot based on predefined setup
			if federationIssuer == "" {
				log.Errorf("no federation issuer set in config")
				return false
			}

			initDedicatedGB := float64(0)
			initOpportunisticGB := float64(0)
			rootLot := Lot{
				LotName: "root",
				Owner:   federationIssuer,
				// A self-parent lot indicates superuser status
				Parents: []string{"root"},
				Paths: []LotPaths{
					{
						Path:      "/",
						Recursive: false,
					},
				},
				MPA: &MPA{
					DedicatedGB:     &initDedicatedGB,
					OpportunisticGB: &initOpportunisticGB,
					MaxNumObjects:   &Int64FromFloat{Value: 0},
					CreationTime:    &Int64FromFloat{Value: 0},
					ExpirationTime:  &Int64FromFloat{Value: 0},
					DeletionTime:    &Int64FromFloat{Value: 0},
				},
			}

			log.Debugf("Creating the root lot defined by %v", rootLot)
			err := CreateLot(&rootLot, federationIssuer)
			if err != nil {
				log.Errorf("error creating root lot: %v", err)
				return false
			}
		}
		log.Infof("Created root lot")
	}


	// Now instantiate any other lots that are in the config
	for _, lot := range initializedLots {
		if lot.LotName != "default" && lot.LotName != "root" {
			err := CreateLot(&lot, federationIssuer)
			if err != nil {
				log.Errorf("error creating  lot: %v", err)
				return false
			}
		}
	}

	log.Infof("LotMan initialization complete")
	lotmanInitSuccess = true
	return true
}

func CreateLot(newLot *Lot, caller string) error {
	// Marshal the JSON into a string for the C function
	lotJSON, err := json.Marshal(*newLot)
	if err != nil {
		return errors.Wrapf(err, "Error marshalling lot JSON: %v", err)
	}

	// Set the context to the incoming lot's owner:
	errMsg := make([]byte, 2048)
	callerMutex.Lock()
	defer callerMutex.Unlock()
	ret := LotmanSetContextStr("caller", caller, &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return fmt.Errorf(fmt.Sprintf("Error creating lot: %s", string(errMsg)))
	}

	// Now finally add the lot
	ret = LotmanAddLot(string(lotJSON), &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return fmt.Errorf(fmt.Sprintf("Error creating lot: %s", string(errMsg)))
	}

	return nil
}

func GetLot(lotName string, recursive bool) (*Lot, error) {
	// Haven't given much thought to these buff sizes yet
	outputBuf := make([]byte, 4096)
	errMsg := make([]byte, 2048)

	ret := LotmanGetLotJSON(lotName, recursive, &outputBuf, &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return nil, errors.Errorf("Error getting lot JSON: %s", string(errMsg))
	}
	trimBuf(&outputBuf)
	var lot Lot
	err := json.Unmarshal(outputBuf, &lot)
	if err != nil {
		return nil, errors.Wrapf(err, "Error unmarshalling lot JSON: %v", err)
	}
	return &lot, nil
}

func UpdateLot(lotUpdate *LotUpdate, caller string) error {
	// Marshal the JSON into a string for the C function
	updateJSON, err := json.Marshal(*lotUpdate)
	if err != nil {
		return errors.Wrapf(err, "Error marshalling lot JSON: %v", err)
	}

	errMsg := make([]byte, 2048)
	callerMutex.Lock()
	defer callerMutex.Unlock()
	ret := LotmanSetContextStr("caller", caller, &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return fmt.Errorf(fmt.Sprintf("Error setting caller for lot update: %s", string(errMsg)))
	}

	ret = LotmanUpdateLot(string(updateJSON), &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return fmt.Errorf(fmt.Sprintf("Error updating lot: %s", string(errMsg)))
	}

	return nil
}

func DeleteLotsRecursive(lotName string, caller string) error {
	errMsg := make([]byte, 2048)
	callerMutex.Lock()
	defer callerMutex.Unlock()
	ret := LotmanSetContextStr("caller", caller, &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return fmt.Errorf(fmt.Sprintf("Error creating lot: %s", string(errMsg)))
	}

	// We've set the caller, now try to delete the lots
	ret = LotmanDeleteLotsRecursive(lotName, &errMsg)
	if ret != 0 {
		trimBuf(&errMsg)
		return fmt.Errorf(fmt.Sprintf("Error deleting lots: %s", string(errMsg)))
	}

	return nil
}
