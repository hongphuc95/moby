package daemon // import "github.com/docker/docker/daemon"

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/backend"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/api/types/versions"
	"github.com/docker/docker/api/types/versions/v1p20"
	"github.com/docker/docker/client"
	"github.com/docker/docker/container"
	"github.com/docker/docker/daemon/stats"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/ioutils"
)

func getAutoRange(ctx context.Context, containerID string) (swarm.AutoRange, string, bool) {
	cli, err := client.NewEnvClient()
	if err != nil {
		return swarm.AutoRange{}, "", false
	}
	defer cli.Close()
	container, err := cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return swarm.AutoRange{}, "", false
	}

	// Swarm labels needed to get AutoRange configuration
	serviceID, serviceName := container.Config.Labels["com.docker.swarm.service.id"], container.Config.Labels["com.docker.swarm.service.name"]
	if serviceID != "" && serviceName != "" {
		resp, _, _ := cli.ServiceInspectWithRaw(ctx, serviceID, types.ServiceInspectOptions{})
		if resp.Spec.AutoRange != nil {
			return resp.Spec.AutoRange, serviceName, true
		}
	}
	return swarm.AutoRange{}, "", false
}

// ContainerStats writes information about the container to the stream
// given in the config object.
func (daemon *Daemon) ContainerStats(ctx context.Context, prefixOrName string, config *backend.ContainerStatsConfig) error {
	// Engine API version (used for backwards compatibility)
	apiVersion := config.Version

	if isWindows && versions.LessThan(apiVersion, "1.21") {
		return errors.New("API versions pre v1.21 do not support stats on Windows")
	}

	ctr, err := daemon.GetContainer(prefixOrName)
	if err != nil {
		return err
	}

	if config.Stream && config.OneShot {
		return errdefs.InvalidParameter(errors.New("cannot have stream=true and one-shot=true"))
	}

	// If the container is either not running or restarting and requires no stream, return an empty stats.
	if (!ctr.IsRunning() || ctr.IsRestarting()) && !config.Stream {
		return json.NewEncoder(config.OutStream).Encode(&types.StatsJSON{
			Name: ctr.Name,
			ID:   ctr.ID,
		})
	}

	// AutoRange initialisation
	if autoRange, serviceName, ok := getAutoRange(ctx, ctr.ID); ok {
		if _, exist := daemon.statsCollector.AutoRangeWatcher[serviceName]; exist {
			if daemon.statsCollector.AutoRangeWatcher[serviceName].Target != ctr {
				daemon.statsCollector.AutoRangeWatcher[serviceName].Target = ctr
				daemon.statsCollector.AutoRangeWatcher[serviceName].UpdateResources()
			}
		} else if _, exist := daemon.statsCollector.AutoRangeWatcher[ctr.ID]; !exist {
			limit := 10 // Size limit of timeserie
			daemon.statsCollector.AutoRangeWatcher[ctr.ID] = &stats.AutoRangeWatcher{
				Config:      autoRange,
				TickRate:    time.Second,
				Target:      ctr,
				ServiceName: serviceName[:strings.LastIndex(serviceName, "_")],
				Input:       make(chan types.StatsJSON, 1),
				Output:      make(chan types.StatsJSON, 1),
				WaitChan:    make(chan bool, 1),
				Obs:         stats.NewObservor(limit),
				Ctx:         ctx,
				Limit:       limit,
				Finished:    false,
			}
			go func() {
				daemon.statsCollector.AutoRangeWatcher[ctr.ID].Watch()
				daemon.statsCollector.AutoRangeWatcher[serviceName] = daemon.statsCollector.AutoRangeWatcher[ctr.ID]
				delete(daemon.statsCollector.AutoRangeWatcher, ctr.ID)
			}()
		} else if !daemon.statsCollector.AutoRangeWatcher[ctr.ID].Finished {
			daemon.statsCollector.AutoRangeWatcher[ctr.ID].SetNewContext(ctx)
		}
	}

	outStream := config.OutStream
	if config.Stream {
		wf := ioutils.NewWriteFlusher(outStream)
		defer wf.Close()
		wf.Flush()
		outStream = wf
	}

	var preCPUStats types.CPUStats
	var preRead time.Time
	getStatJSON := func(v interface{}) *types.StatsJSON {
		ss := v.(types.StatsJSON)
		ss.Name = ctr.Name
		ss.ID = ctr.ID
		ss.PreCPUStats = preCPUStats
		ss.PreRead = preRead
		preCPUStats = ss.CPUStats
		preRead = ss.Read
		return &ss
	}

	enc := json.NewEncoder(outStream)

	updates := daemon.subscribeToContainerStats(ctr)
	defer daemon.unsubscribeToContainerStats(ctr, updates)

	noStreamFirstFrame := !config.OneShot

	var oldStats *types.StatsJSON
	first := true

	for {
		select {
		case v, ok := <-updates:
			if !ok {
				return nil
			}

			var statsJSON interface{}
			statsJSONPost120 := getStatJSON(v)
			if first {
				oldStats = statsJSONPost120
				first = false
			}
			if _, exist := daemon.statsCollector.AutoRangeWatcher[ctr.ID]; exist {
				if !daemon.statsCollector.AutoRangeWatcher[ctr.ID].Finished {
					select {
					case daemon.statsCollector.AutoRangeWatcher[ctr.ID].Input <- *statsJSONPost120:
					default:
					}

					select {
					case up, ok := <-daemon.statsCollector.AutoRangeWatcher[ctr.ID].Output:
						if !ok {
							return nil
						}

						statsJSONPost120 = &up
						oldStats = statsJSONPost120
					default:
						statsJSONPost120 = oldStats
					}
				} else {
					statsJSONPost120.AutoRange = stats.ConvertAutoRange(daemon.statsCollector.AutoRangeWatcher[ctr.ID].Config)
				}
			}
			if versions.LessThan(apiVersion, "1.21") {
				var (
					rxBytes   uint64
					rxPackets uint64
					rxErrors  uint64
					rxDropped uint64
					txBytes   uint64
					txPackets uint64
					txErrors  uint64
					txDropped uint64
				)
				for _, v := range statsJSONPost120.Networks {
					rxBytes += v.RxBytes
					rxPackets += v.RxPackets
					rxErrors += v.RxErrors
					rxDropped += v.RxDropped
					txBytes += v.TxBytes
					txPackets += v.TxPackets
					txErrors += v.TxErrors
					txDropped += v.TxDropped
				}
				statsJSON = &v1p20.StatsJSON{
					Stats: statsJSONPost120.Stats,
					Network: types.NetworkStats{
						RxBytes:   rxBytes,
						RxPackets: rxPackets,
						RxErrors:  rxErrors,
						RxDropped: rxDropped,
						TxBytes:   txBytes,
						TxPackets: txPackets,
						TxErrors:  txErrors,
						TxDropped: txDropped,
					},
				}
			} else {
				statsJSON = statsJSONPost120
			}

			if !config.Stream && noStreamFirstFrame {
				// prime the cpu stats so they aren't 0 in the final output
				noStreamFirstFrame = false
				continue
			}

			if err := enc.Encode(statsJSON); err != nil {
				return err
			}

			if !config.Stream {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (daemon *Daemon) subscribeToContainerStats(c *container.Container) chan interface{} {
	return daemon.statsCollector.Collect(c)
}

func (daemon *Daemon) unsubscribeToContainerStats(c *container.Container, ch chan interface{}) {
	daemon.statsCollector.Unsubscribe(c, ch)
}

// GetContainerStats collects all the stats published by a container
func (daemon *Daemon) GetContainerStats(container *container.Container) (*types.StatsJSON, error) {
	stats, err := daemon.stats(container)
	if err != nil {
		return nil, err
	}

	// We already have the network stats on Windows directly from HCS.
	if !container.Config.NetworkDisabled && runtime.GOOS != "windows" {
		if stats.Networks, err = daemon.getNetworkStats(container); err != nil {
			return nil, err
		}
	}

	return stats, nil
}
