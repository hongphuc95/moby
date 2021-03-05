package stats // import "github.com/docker/docker/daemon/stats"

/*
 #AutoRange
 a feature that help the user predict and apply the best limits to his services.
 	Why?
 This collector extension was thought as a way to monitor and predict the optimal configuration
 for a service.
 The goal was to find the point where a service could function properly, but still save as much
 resources as possible, by monitoring activity and deducing optimal values.
 It was written as a way to answer the question
	 `How to optimise the number of services running on our infrastructure without losing quality of service?`
 How?
 It uses swarm labels and require swarm mode to be enabled (see #improvements).
 The logic behind the feature can be described in 3 points:
 - First, we collect the metrics and apply transformations on it to generate two values.
 Those values represent a “box” around the actual consumption.
 - Then, we transform these values into timeseries, using some of the keydata collected previously to weight our operations.
 The amplitude of change between values is monitored to know if it’s a good time to stop measurements.
 - Finally, we obtain refined values that we apply as limitation to the service.
 The data is then kept in a reduced form to limit memory usage.
 The functionnality is declared by adding the autorange key to the docker-compose.yml.
 The mechanism works for cpu% and memory, with or without basevalues.
 Below is an example of both.
 autorange:
 memory:
 cpu%:
 The available keys are:- min (in octets)- max (in octets)- threshold% (only for memory, represents a security margin that will be refined by the algorithm)
 autorange:
 memory:
         min: "110000"
 		   max: "120000"
 		   threshold%: "10"
 cpu%:
 		   min: "60"
 		   max: "70"
 This functionality is deployed with docker stack deploy --compose-file=/your/compose/file and
 then docker container stats --format AutoRange(format is not necessary but shows the predicted values).
          The `docker container stats` command is mandatory to start and keep running the collector.
 You can always leave the docker container stats screen and
 come back later, the mechanism will be paused and the accumulated datas won’t be lost.
*/

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	ctn "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/docker/docker/container"
	"github.com/sirupsen/logrus"
)

const (
	kiB                   = 1024
	miB                   = 1024 * kiB
	minAllowedMemoryLimit = 5 * miB
)

// PredictedValueRAM is a struct that holds the values used to generate the memory limits
type PredictedValueRAM struct {
	min, max, threshold []uint64
}

// TimeSerieRAM is a struct that holds everything needed for memory limit generation
type TimeSerieRAM struct {
	min, max, usage, highest, lowest, amplitude []uint64
	timestamps                                  []time.Time
	started                                     time.Time
	PredictedValues                             PredictedValueRAM
	MemoryPrediction                            bool
}

// PredictedValueCPU is a struct that holds the values used to generate the CPU limits
type PredictedValueCPU struct {
	percent, usage []float64
}

// TimeSerieCPU is a struct that holds everything needed for CPU limit generation
type TimeSerieCPU struct {
	percent, usage  []float64
	timestamps      []time.Time
	started         time.Time
	PredictedValues PredictedValueCPU
	CPUPrediction   bool
}

// Observor is the struct that holds the timeserie for the watcher
type Observor struct {
	TimeSerieRAM
	TimeSerieCPU
}

// AutoRangeWatcher is the struct that holds everything required to enable autorange functionnality
type AutoRangeWatcher struct {
	Output, Input chan types.StatsJSON
	WaitChan      chan bool
	TickRate      time.Duration
	Config        swarm.AutoRange
	Target        *container.Container
	ServiceName   string
	Obs           *Observor
	Ctx           context.Context
	Limit         int

	Finished, started bool
}

// NewObservor returns a newly initialized observor that will be used by the watcher
func NewObservor(size int) *Observor {
	return &Observor{
		TimeSerieRAM: TimeSerieRAM{
			min:              make([]uint64, 0, size),
			max:              make([]uint64, 0, size),
			usage:            make([]uint64, 0, size),
			highest:          make([]uint64, 0, size),
			lowest:           make([]uint64, 0, size),
			amplitude:        make([]uint64, 0, size),
			MemoryPrediction: true,
			PredictedValues: PredictedValueRAM{
				min:       make([]uint64, 0, size),
				max:       make([]uint64, 0, size),
				threshold: make([]uint64, 0, size),
			},
		},
		TimeSerieCPU: TimeSerieCPU{
			percent:       make([]float64, 0, size),
			usage:         make([]float64, 0, size),
			CPUPrediction: true,
			PredictedValues: PredictedValueCPU{
				percent: make([]float64, 0, size),
				usage:   make([]float64, 0, size),
			},
		},
	}
}

func fifoUint(array []uint64, value uint64, size int) []uint64 {
	if size == 0 {
		return array
	}

	start := 0
	if len(array) >= size {
		start = 1
	}
	return append(array[start:], value)
}

func fifoFloat(array []float64, value float64, size int) []float64 {
	if size == 0 {
		return array
	}

	start := 0
	if len(array) >= size {
		start = 1
	}
	return append(array[start:], value)
}

func lowestOf(array []uint64) (lowest uint64) {
	if len(array) <= 0 {
		lowest = 1
		return
	}
	lowest = array[0]

	for _, value := range array {
		if value < lowest {
			lowest = value
		}
	}
	return
}

func highestOf(array []uint64) (highest int) {
	if len(array) <= 0 {
		highest = 0
		return
	}
	best := array[0]

	for _, value := range array {
		if value > best {
			best = value
		}
	}
	highest = int(best)
	return
}

func percent(value int) (percent int) {
	if value == 0 {
		return value
	}
	percent = value / 100
	return
}

func percentageBetween(this, that int) (delta int) {
	if this == 0 {
		return this
	}
	diff := that - this
	delta = int((float64(diff) / float64(this)) * 100)
	return
}

// SetNewContext helps for the start and stops of the collector
func (ar *AutoRangeWatcher) SetNewContext(ctx context.Context) {
	ar.Ctx = ctx
	ar.WaitChan <- true
}

// UpdateResources is the function that handle the verification and application of the generated limits
func (ar *AutoRangeWatcher) UpdateResources() {
	var update ctn.UpdateConfig

	if ar.IsActivated("memoryAR") {

		config := ar.Config["memoryAR"]

		sugMin, _ := strconv.Atoi(config["nmin"])
		sugMax, _ := strconv.Atoi(config["nmax"])
		threshold, _ := strconv.Atoi(config["opti"])

		// One last sum with the highest usage to smooth the prediction and reduce the
		// error probability. It's generaly a subtle ajustement.
		// The docker daemon does not permit memory limit lesser than 6mb

		var highest int
		var lowest uint64
		if ar.Obs != nil {
			lowest = lowestOf(ar.Obs.TimeSerieRAM.lowest)
			highest = highestOf(ar.Obs.TimeSerieRAM.highest)
		} else {
			lowest = uint64(sugMin)
			highest = sugMax
		}

		if threshold < 10 {
			threshold = 10
		}

		update.Resources.Memory = int64(sugMax + percent(highest)*(threshold*2))
		if update.Resources.Memory < minAllowedMemoryLimit {
			update.Resources.Memory = minAllowedMemoryLimit + miB
		}

		update.Resources.MemorySwap = -1

		// Here we do pretty much the same as above, to further refine the limit and better fit
		// the observed consumption
		update.Resources.MemoryReservation = int64((uint64(sugMin) + lowest)) / 2
		if update.Resources.MemoryReservation < minAllowedMemoryLimit {
			update.Resources.MemoryReservation = minAllowedMemoryLimit + 5*miB
		}

		if update.Resources.MemoryReservation > update.Resources.Memory {
			update.Resources.MemoryReservation, update.Resources.Memory = update.Resources.Memory, update.Resources.MemoryReservation
		}

		ar.Config["memoryAR"]["sugmin"] = strconv.Itoa(int(update.Resources.MemoryReservation))
		ar.Config["memoryAR"]["sugmax"] = strconv.Itoa(int(update.Resources.Memory))

	}

	if ar.IsActivated("cpuAR") {

		sugMax, _ := strconv.Atoi(ar.Config["cpuAR"]["usageOpti"])

		update.Resources.CPURealtimeRuntime = int64(sugMax)
		update.Resources.CpusetCpus, ar.Config["cpuAR"]["numCPU"] = CPUUsageToConfig(ar.Config["cpuAR"]["percentOpti"])

	}

	// Updating is done using the docker client API
	cli, err := client.NewEnvClient()
	if err != nil {
		logrus.Errorf("%v\n", err)
		return
	}

	timer := time.Second * 30
	ticker := time.NewTicker(timer)
	count := 10
	baseCount := count
	for ; true; <-ticker.C {
		_, err = cli.ContainerUpdate(ar.Ctx, ar.Target.ID, update)
		if err == nil {
			logrus.Infof("container: %s (service: %s) now has limits applicated\n", ar.Target.Name, ar.ServiceName)
			break
		} else if count == 0 {
			logrus.Errorf("failed to update container with new limits after %d attempt", baseCount)
			break
		} else {
			logrus.Errorf("%v\nretrying in %v..", err, timer)
		}
		count--
	}

	return
}

// IsActivated returns a true if category is found in config
func (ar *AutoRangeWatcher) IsActivated(category string) bool {
	_, exist := ar.Config[category]
	return exist
}

// Continue checks if the loop should continue for the given category
func continueIteration(category, value string, done bool) bool {
	if strings.Compare(category, value) == 0 && !done {
		return true
	}
	return false
}

// CPUUsageToConfig convert usage as a string to docker cpu config string and number to display
func CPUUsageToConfig(usage string) (config, number string) {
	fUsage, _ := strconv.ParseFloat(usage, 32)
	if fUsage <= 0 {
		return
	}

	n := 1 + int(fUsage/100)
	for i := 0; i < n; i++ {
		config += strconv.Itoa(i)
		if i+1 < n {
			config += ","
		}
	}
	number = strconv.Itoa(n)
	return
}

func (ar *AutoRangeWatcher) baseValueMemory() (min, max, threshold int) {
	if ar.IsActivated("memory") {
		min, _ = strconv.Atoi(ar.Config["memory"]["min"])
		if min < 10000 {
			min = 10000
		}

		max, _ = strconv.Atoi(ar.Config["memory"]["max"])
		if max < 20000 {
			max = 20000
		}

		threshold, _ = strconv.Atoi(ar.Config["memory"]["threshold"])
		if threshold == 0 {
			threshold = 30
		}
		ar.Config["memoryAR"] = make(map[string]string)
	} else {
		min, max, threshold = 0, 0, 0
	}
	return
}

func (ar *AutoRangeWatcher) baseValueCPU() (cpuMin, cpuMax int) {
	if ar.IsActivated("cpu%") {
		cpuMin, _ = strconv.Atoi(ar.Config["cpu%"]["min"])
		cpuMax, _ = strconv.Atoi(ar.Config["cpu%"]["max"])
		ar.Config["cpuAR"] = make(map[string]string)
	} else {
		cpuMin, cpuMax = 0, 0
	}
	return
}

func (ar *AutoRangeWatcher) isInBadState() bool {
	return (ar.Target.State.Dead || !ar.Target.State.Running)
}

func (ar *AutoRangeWatcher) isStarted() bool {
	return ar.started
}

func (ar *AutoRangeWatcher) isFinished() bool {
	return (ar.Obs.TimeSerieRAM.MemoryPrediction && ar.Obs.TimeSerieCPU.CPUPrediction && !ar.Finished)
}

func getExtremeValues(usage, lowest, highest uint64) (uint64, uint64) {
	if usage < lowest {
		lowest = usage
	} else if usage > highest {
		highest = usage
	}
	return lowest, highest
}

func checkMemoryEndCondition(lenSerie, limit int, mediumAmplitude uint64) bool {
	if lenSerie >= limit || (lenSerie > limit/2 && mediumAmplitude <= 2) {
		return true
	}
	return false
}

func (ar *AutoRangeWatcher) startRoutine(ncpus uint32, cpuMin, cpuMax int) {
	if ar.IsActivated("memory") {
		ar.Obs.TimeSerieRAM.MemoryPrediction = false
	}

	if ar.IsActivated("cpu%") {
		ar.Obs.TimeSerieCPU.CPUPrediction = false
	}

	if cpuMin != 0 && cpuMax != 0 {
		fifoFloat(ar.Obs.TimeSerieCPU.percent, float64(((cpuMin+cpuMax)/2)/int(ncpus)), ar.Limit)
	}
	ar.started = true
}

// Watch is the function that will keep the goroutine alive, process the metrics and generate the time series
func (ar *AutoRangeWatcher) Watch() {
	var (
		input                                types.StatsJSON
		lowest, highest, oldUsage, oldSystem uint64 = 0, 0, 0, 0
		cpuTurn, memoryTurn                  int    = 0, 0
	)

	// Recover base config, those values will be used as base values
	// If no base config is provided, it use dummy values as starter
	min, max, threshold := ar.baseValueMemory()

	cpuMin, cpuMax := ar.baseValueCPU()

	// Initialisation time
	ticker := time.NewTicker(ar.TickRate)
	time.Sleep(ar.TickRate)
	ar.started = false

	logrus.Infof("container: %s (service: %s) started with activated autorange", ar.Target.Name, ar.ServiceName)
	for range ticker.C {
		select {
		case in := <-ar.Input:
			input = in
		case <-ar.Ctx.Done(): // Handler for signal interrupt
			<-ar.WaitChan
			continue
		}

		// Healthchecking is required before every loops to ensure data integrity
		// We don't want false prediction because the container was offline
		if ar.isInBadState() {
			logrus.Infof("container: %s (service: %s) exited, removing autorange", ar.Target.Name, ar.ServiceName)
			return
		}

		// Initalisation / End routines
		if !ar.isStarted() {
			input.Stats.MemoryStats.MaxUsage, lowest = input.Stats.MemoryStats.Usage, input.Stats.MemoryStats.Usage
			ar.startRoutine(input.Stats.CPUStats.OnlineCPUs, cpuMin, cpuMax)

		} else if ar.isFinished() {
			ar.UpdateResources()
			ar.Finished = true
			return
		}

		for category := range ar.Config {
			if continueIteration(category, "memory", ar.Obs.TimeSerieRAM.MemoryPrediction) {

				// Follow memory usage and change min and max accordingly.
				// These values represent the "bearings" around the usage value
				min, max = processMemoryStats(input.Stats.MemoryStats.Usage, min, max, threshold)

				// Always get the lowest and highest point in the serie,
				// as we'll use them for weighting purposes
				lowest, highest = getExtremeValues(input.Stats.MemoryStats.Usage, lowest, highest)

				ar.Obs.TimeSerieRAM.min = fifoUint(ar.Obs.TimeSerieRAM.min, uint64(min), ar.Limit)
				ar.Obs.TimeSerieRAM.max = fifoUint(ar.Obs.TimeSerieRAM.max, uint64(max), ar.Limit)
				ar.Obs.TimeSerieRAM.usage = fifoUint(ar.Obs.TimeSerieRAM.usage, input.Stats.MemoryStats.Usage, ar.Limit)

				// Timeserie arrays are ready to be processed
				if memoryTurn >= ar.Limit {
					memoryTurn = 0

					// Stats about the serie
					// Amplitude represent the space between lowest and highest
					ar.Obs.TimeSerieRAM.highest = fifoUint(ar.Obs.TimeSerieRAM.highest, highest, ar.Limit)
					ar.Obs.TimeSerieRAM.lowest = fifoUint(ar.Obs.TimeSerieRAM.lowest, lowest, ar.Limit)
					ar.Obs.TimeSerieRAM.amplitude = fifoUint(ar.Obs.TimeSerieRAM.amplitude, highest/lowest, ar.Limit)

					// Generate predicted values
					aMin, aMax := averrage(ar.Obs.TimeSerieRAM.min), averrage(ar.Obs.TimeSerieRAM.max)
					aMin = aMin + (aMin/100)*uint64(percentageBetween(int(aMin), int(lowest)))
					aMax = aMax + (aMax/100)*uint64(percentageBetween(int(aMax), int(highest)))

					// Stock predicted values
					ar.Obs.TimeSerieRAM.PredictedValues.min = fifoUint(ar.Obs.TimeSerieRAM.PredictedValues.min, aMin, ar.Limit)
					ar.Obs.TimeSerieRAM.PredictedValues.max = fifoUint(ar.Obs.TimeSerieRAM.PredictedValues.max, aMax, ar.Limit)

					highest, lowest = 0, input.Stats.MemoryStats.Usage

					// When the number of timeseries is big enough, or if the rate of change <= 2
					// we can assume that the optimal limits can be calculated
					medAmplitude, lenSerie := averrage(ar.Obs.TimeSerieRAM.amplitude), len(ar.Obs.TimeSerieRAM.PredictedValues.min)
					ar.Obs.TimeSerieRAM.PredictedValues.threshold = fifoUint(ar.Obs.TimeSerieRAM.PredictedValues.threshold, medAmplitude, ar.Limit)
					threshold = int(weightedAverrage(ar.Obs.TimeSerieRAM.PredictedValues.threshold, generateMemoryWeight(ar.Obs.TimeSerieRAM.PredictedValues.threshold, ar.Obs.TimeSerieRAM.PredictedValues.threshold)))

					ar.Obs.TimeSerieRAM.MemoryPrediction = checkMemoryEndCondition(lenSerie, ar.Limit, medAmplitude)

					// Display result
					avMin := weightedAverrage(ar.Obs.TimeSerieRAM.PredictedValues.min, generateMemoryWeight(ar.Obs.TimeSerieRAM.PredictedValues.min, ar.Obs.TimeSerieRAM.lowest))
					avMax := weightedAverrage(ar.Obs.TimeSerieRAM.PredictedValues.max, generateMemoryWeight(ar.Obs.TimeSerieRAM.PredictedValues.max, ar.Obs.TimeSerieRAM.highest))
					ar.Config["memoryAR"]["nmin"] = strconv.Itoa(avMin + percent(avMin)*threshold)
					ar.Config["memoryAR"]["nmax"] = strconv.Itoa(avMax + percent(avMax)*threshold)
					ar.Config["memoryAR"]["opti"] = strconv.Itoa(threshold)
					ar.Config["memoryAR"]["usage"] = strconv.Itoa(int(input.Stats.MemoryStats.Usage))
					continue
				}
				memoryTurn++

			} else if continueIteration(category, "cpu%", ar.Obs.TimeSerieCPU.CPUPrediction) {

				// The logic for the cpu loop is pretty much the same as memory, but more focused
				// on cpu cores

				// Generate CPU percent
				deltaUsage := float64(input.Stats.CPUStats.CPUUsage.TotalUsage) - float64(oldUsage)
				deltaSystem := float64(input.Stats.CPUStats.SystemUsage) - float64(oldSystem)
				numCPUs := float64(input.Stats.CPUStats.OnlineCPUs)
				CPUPercent := (deltaUsage / deltaSystem) * numCPUs * 100.0

				ar.Obs.TimeSerieCPU.percent = fifoFloat(ar.Obs.TimeSerieCPU.percent, CPUPercent, ar.Limit)
				ar.Obs.TimeSerieCPU.usage = fifoFloat(ar.Obs.TimeSerieCPU.usage, deltaUsage, ar.Limit)

				// Timeserie arrays are ready to be processed
				if cpuTurn >= ar.Limit {
					cpuTurn = 0

					avPercent, avUsage := averrageFloat(ar.Obs.TimeSerieCPU.percent), averrageFloat(ar.Obs.TimeSerieCPU.usage)

					ar.Obs.TimeSerieCPU.PredictedValues.percent = fifoFloat(ar.Obs.TimeSerieCPU.PredictedValues.percent, avPercent, ar.Limit)
					ar.Obs.TimeSerieCPU.PredictedValues.usage = fifoFloat(ar.Obs.TimeSerieCPU.PredictedValues.usage, avUsage, ar.Limit)
					if len(ar.Obs.TimeSerieCPU.PredictedValues.percent) >= ar.Limit {
						cBestPercent := averrageFloat(ar.Obs.TimeSerieCPU.PredictedValues.percent)
						cBestUsage := averrageFloat(ar.Obs.TimeSerieCPU.PredictedValues.usage)

						// Display
						ar.Config["cpuAR"]["percentOpti"] = strconv.FormatFloat(cBestPercent, 'f', 3, 64)
						ar.Config["cpuAR"]["usageOpti"] = strconv.FormatFloat(cBestUsage, 'f', 0, 64)
						ar.Obs.TimeSerieCPU.CPUPrediction = true

					}
					continue
				}
				cpuTurn++
				oldSystem, oldUsage = input.Stats.CPUStats.SystemUsage, input.Stats.CPUStats.CPUUsage.TotalUsage
			}
		}

		input.AutoRange = ConvertAutoRange(ar.Config)
		select {
		case ar.Output <- input:
		default:
		}
	}
}

func generateMemoryWeight(array, highestArray []uint64) []float32 {
	weight := make([]float32, 0, len(array))

	highest := highestOf(highestArray)
	if highest == 0 {
		return weight
	}

	for _, number := range array {
		if number == 0 {
			break
		}
		distance := float32((uint64(highest) / number))
		toAdd := 1 / distance
		if math.IsInf(float64(toAdd), 1) {
			toAdd = 1.0
		}
		weight = append(weight, toAdd)
	}
	return weight
}

func weightedAverrage(array []uint64, weight []float32) (averrage int) {
	var total int

	if len(array) <= 0 || len(weight) <= 0 {
		return 0
	}

	for index, number := range array {
		total += int(float32(number) / weight[index])
	}
	averrage = total / len(array)
	return
}

func averrageFloat(array []float64) float64 {
	arrayLen := len(array)
	if arrayLen <= 0 {
		return 0
	}

	var total float64 = 0
	for _, number := range array {
		if math.IsNaN(number) {
			arrayLen--
			continue
		}
		total += number
	}
	return total / float64(arrayLen)
}

func averrage(array []uint64) (total uint64) {
	arrayLen := len(array)
	if arrayLen <= 0 {
		return 0
	}

	for _, number := range array {
		total += number
	}
	total = total / uint64(arrayLen)
	return
}

func processMemoryStats(mUsage uint64, min, max, threshold int) (int, int) {
	usage := int(mUsage)

	if usage > (min+percent(max-min)/100)*threshold {

		distance := percentageBetween(min, usage)

		min += distance * percent(min)
		max = min + threshold*percent(min)

	} else if usage < (min-percent(max-min)/100)*threshold {

		min = usage + threshold*percent(usage)
		max = min + threshold*percent(min)

	}

	return min, max
}

// ConvertAutoRange is a function that is used to convert from swarm.AutoRange to types.AutoRange
func ConvertAutoRange(autoRange swarm.AutoRange) types.AutoRange {
	ar := make(types.AutoRange)
	for key := range autoRange {
		ar[key] = make(map[string]string)
		for subKey, subValue := range autoRange[key] {
			ar[key][subKey] = subValue
		}
	}
	return ar
}
