package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
)

//////////////////
// CONFIG TYPES //
//////////////////

// Config stores the global configuration for the scheduler plugin.
//
// It is parsed from a JSON file in a separate ConfigMap.
type Config struct {
	// Scoring defines our policies around how to weight where Pods should be scheduled.
	Scoring ScoringConfig `json:"scoring"`

	// Watermark is the fraction of total resources allocated above which we should be migrating VMs
	// away to reduce usage.
	Watermark float64 `json:"watermark"`

	// SchedulerName informs the scheduler of its name, so that it can identify pods that a previous
	// version handled.
	SchedulerName string `json:"schedulerName"`

	// ReconcileWorkers sets the number of parallel workers to use for the global reconcile queue.
	ReconcileWorkers int `json:"reconcileWorkers"`

	// LogSuccessiveFailuresThreshold is the threshold for number of failures in a row at which
	// we'll start logging that an object is failing to be reconciled.
	//
	// This is to help make it easier to go from metrics saying "N objects are failing" to actually
	// finding the relevant objects.
	LogSuccessiveFailuresThreshold int `json:"logSuccessiveFailuresThreshold"`

	// StartupEventHandlingTimeoutSeconds gives the maximum duration, in seconds, that we are
	// allowed to wait to finish handling all of the initial events generated by reading the cluster
	// state on startup.
	//
	// If event processing takes longer than this time, then plugin creation will fail, and the
	// scheduler pod will retry.
	StartupEventHandlingTimeoutSeconds int `json:"startupEventHandlingTimeoutSeconds"`

	// K8sCRUDTimeoutSeconds sets the timeout to use for creating, updating, or deleting singular
	// kubernetes objects.
	K8sCRUDTimeoutSeconds int `json:"k8sCRUDTimeoutSeconds"`

	// PatchRetryWaitSeconds sets the minimum duration, in seconds, that we must wait between
	// successive patch operations on a VirtualMachine object.
	PatchRetryWaitSeconds int `json:"patchRetryWaitSeconds"`

	// NodeMetricLabels gives additional labels to annotate node metrics with.
	// The map is keyed by the metric name, and gives the kubernetes label that should be used to
	// populate it.
	//
	// For example, we might use the following:
	//
	//   {
	//     "availability_zone": "topology.kubernetes.io/zone",
	//     "node_group": "eks.amazonaws.com/nodegroup"
	//   }
	NodeMetricLabels map[string]string `json:"nodeMetricLabels"`

	// IgnoredNamespaces, if provided, gives a list of namespaces that the plugin should completely
	// ignore, as if pods from those namespaces do not exist.
	//
	// This is specifically designed for our "overprovisioning" namespace, which creates paused pods
	// to trigger cluster-autoscaler.
	//
	// The only exception to this rule is during Filter method calls, where we do still count the
	// resources from such pods. The reason to do that is so that these overprovisioning pods can be
	// evicted, which will allow cluster-autoscaler to trigger scale-up.
	IgnoredNamespaces []string `json:"ignoredNamespaces"`
}

type ScoringConfig struct {
	// Details about node scoring:
	// See also: https://www.desmos.com/calculator/wg8s0yn63s
	// In the desmos, the value f(x,s) gives the score (from 0 to 1) of a node that's x amount full
	// (where x is a fraction from 0 to 1), with a total size that is equal to the maximum size node
	// times s (i.e. s (or: "scale") gives the ratio between this nodes's size and the biggest one).

	// MinUsageScore gives the ratio of the score at the minimum usage (i.e. 0) relative to the
	// score at the midpoint, which will have the maximum.
	//
	// This corresponds to y₀ in the desmos link above.
	MinUsageScore float64 `json:"minUsageScore"`
	// MaxUsageScore gives the ratio of the score at the maximum usage (i.e. full) relative to the
	// score at the midpoint, which will have the maximum.
	//
	// This corresponds to y₁ in the desmos link above.
	MaxUsageScore float64 `json:"maxUsageScore"`
	// ScorePeak gives the fraction at which the "target" or highest score should be, with the score
	// sloping down on either side towards MinUsageScore at 0 and MaxUsageScore at 1.
	//
	// This corresponds to xₚ in the desmos link.
	ScorePeak float64 `json:"scorePeak"`

	// Randomize, if true, will cause the scheduler to score a node with a random number in the
	// range [minScore + 1, trueScore], instead of the trueScore.
	Randomize bool
}

///////////////////////
// CONFIG VALIDATION //
///////////////////////

// if the returned error is not nil, the string is a JSON path to the invalid value
func (c *Config) validate() (string, error) {
	if path, err := c.Scoring.validate(); err != nil {
		return fmt.Sprintf("nodeConfig.%s", path), err
	}

	if c.SchedulerName == "" {
		return "schedulerName", errors.New("string cannot be empty")
	}

	if c.ReconcileWorkers <= 0 {
		return "reconcileWorkers", errors.New("value must be > 0")
	}

	if c.LogSuccessiveFailuresThreshold <= 0 {
		return "logSuccessiveFailuresThreshold", errors.New("value must be > 0")
	}

	if c.StartupEventHandlingTimeoutSeconds <= 0 {
		return "startupEventHandlingTimeoutSeconds", errors.New("value must be > 0")
	}

	if c.K8sCRUDTimeoutSeconds <= 0 {
		return "k8sCRUDTimeoutSeconds", errors.New("value must be > 0")
	}

	if c.PatchRetryWaitSeconds <= 0 {
		return "patchRetryWaitSeconds", errors.New("value must be > 0")
	}

	if c.Watermark <= 0.0 {
		return "watermark", errors.New("value must be > 0")
	} else if c.Watermark > 1.0 {
		return "watermark", errors.New("value must be <= 1")
	}

	return "", nil
}

func (c *ScoringConfig) validate() (string, error) {
	if c.MinUsageScore < 0 || c.MinUsageScore > 1 {
		return "minUsageScore", errors.New("value must be between 0 and 1, inclusive")
	} else if c.MaxUsageScore < 0 || c.MaxUsageScore > 1 {
		return "maxUsageScore", errors.New("value must be between 0 and 1, inclusive")
	} else if c.ScorePeak < 0 || c.ScorePeak > 1 {
		return "scorePeak", errors.New("value must be between 0 and 1, inclusive")
	}

	return "", nil
}

////////////////////
// CONFIG READING //
////////////////////

const DefaultConfigPath = "/etc/scheduler-plugin-config/autoscale-enforcer-config.json"

func ReadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Error opening config file %q: %w", path, err)
	}

	defer file.Close()
	var config Config
	jsonDecoder := json.NewDecoder(file)
	jsonDecoder.DisallowUnknownFields()
	if err = jsonDecoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("Error decoding JSON config in %q: %w", path, err)
	}

	if path, err = config.validate(); err != nil {
		return nil, fmt.Errorf("Invalid config at %s: %w", path, err)
	}

	return &config, nil
}

//////////////////////////////////////
// HELPER METHODS FOR USING CONFIGS //
//////////////////////////////////////

func (c Config) ignoredNamespace(namespace string) bool {
	return slices.Contains(c.IgnoredNamespaces, namespace)
}
