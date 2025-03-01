package metrics

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/lavanet/lava/utils"
)

type AggregatedMetric struct {
	TotalLatency int64
	RelaysCount  int64
	SuccessCount int64
}

type MetricService struct {
	AggregatedMetricMap *map[string]map[string]map[string]*AggregatedMetric
	MetricsChannel      chan RelayMetrics
	ReportUrl           string
}

func NewMetricService() *MetricService {
	reportMetricsUrl := os.Getenv("REPORT_METRICS_URL")
	intervalData := os.Getenv("METRICS_INTERVAL_FOR_SENDING_DATA_MIN")
	if reportMetricsUrl == "" || intervalData == "" {
		return nil
	}
	intervalForMetrics, _ := strconv.ParseInt(intervalData, 10, 32)
	metricChannelBufferSizeData := os.Getenv("METRICS_BUFFER_SIZE_NR")
	metricChannelBufferSize, _ := strconv.ParseInt(metricChannelBufferSizeData, 10, 64)
	mChannel := make(chan RelayMetrics, metricChannelBufferSize)
	result := &MetricService{
		MetricsChannel:      mChannel,
		ReportUrl:           reportMetricsUrl,
		AggregatedMetricMap: &map[string]map[string]map[string]*AggregatedMetric{},
	}

	// setup reader & sending of the results via http
	ticker := time.NewTicker(time.Duration(intervalForMetrics * time.Minute.Nanoseconds()))
	go func() {
		for {
			select {
			case <-ticker.C:
				{
					utils.LavaFormatInfo("metric triggered, sending accumulated data to server", nil)
					result.SendEachProjectMetricData()
				}
			case metricData := <-mChannel:
				utils.LavaFormatInfo("reading from chanel data", nil)
				result.storeAggregatedData(metricData)
			}
		}
	}()
	return result
}

func (m *MetricService) SendData(data RelayMetrics) {
	if m.MetricsChannel != nil {
		select {
		case m.MetricsChannel <- data:
		default:
			utils.LavaFormatInfo("channel is full, ignoring these data", &map[string]string{
				"projectHash": data.ProjectHash,
				"chainId":     data.ChainID,
				"apiType":     data.APIType,
			})
		}
	}
}

func (m *MetricService) SendEachProjectMetricData() {
	if m.AggregatedMetricMap == nil {
		return
	}

	for projectKey, projectData := range *m.AggregatedMetricMap {
		toSendData := prepareArrayForProject(projectData, projectKey)
		go sendMetricsViaHttp(m.ReportUrl, toSendData)
	}
	// we reset to be ready for new metric data
	m.AggregatedMetricMap = &map[string]map[string]map[string]*AggregatedMetric{}
}

func prepareArrayForProject(projectData map[string]map[string]*AggregatedMetric, projectKey string) []RelayAnalyticsDTO {
	var toSendData []RelayAnalyticsDTO
	for chainKey, chainData := range projectData {
		for apiTypekey, apiTypeData := range chainData {
			toSendData = append(toSendData, RelayAnalyticsDTO{
				ProjectHash:  projectKey,
				APIType:      apiTypekey,
				ChainID:      chainKey,
				Latency:      apiTypeData.TotalLatency / apiTypeData.RelaysCount, // we loose the precise during this, and this would never be 0 if we have any record on this project
				RelayCounts:  apiTypeData.RelaysCount,
				SuccessCount: apiTypeData.SuccessCount,
			})
		}
	}
	return toSendData
}

func sendMetricsViaHttp(reportUrl string, data []RelayAnalyticsDTO) error {
	if len(data) == 0 {
		utils.LavaFormatDebug("no metrics found for this project.", nil)
		return nil
	}
	jsonValue, err := json.Marshal(data)
	if err != nil {
		utils.LavaFormatError("error converting data to json", err, nil)
		return err
	}
	resp, err := http.Post(reportUrl, "application/json", bytes.NewBuffer(jsonValue))
	if err != nil {
		utils.LavaFormatError("error posting data to report url.", err, &map[string]string{"url": reportUrl})
		return err
	}
	if resp.StatusCode != http.StatusOK {
		utils.LavaFormatError("error status code returned from server.", nil, &map[string]string{"url": reportUrl})
	}
	return nil
}

func (m *MetricService) storeAggregatedData(data RelayMetrics) error {
	utils.LavaFormatDebug("new data to store", &map[string]string{
		"projectHash": data.ProjectHash,
		"apiType":     data.APIType,
		"chainId":     data.ChainID,
	})

	var successCount int64
	if data.Success {
		successCount = 1
	}

	store := *m.AggregatedMetricMap // for simplicity during operations
	projectData, exists := store[data.ProjectHash]
	if exists {
		m.storeChainIdData(projectData, data, successCount)
	} else {
		// means we haven't stored any data yet for this project, so we build all the maps
		projectData = map[string]map[string]*AggregatedMetric{
			data.ChainID: {
				data.APIType: &AggregatedMetric{
					TotalLatency: data.Latency,
					RelaysCount:  1,
					SuccessCount: successCount,
				},
			},
		}
		store[data.ProjectHash] = projectData
	}
	return nil
}

func (m *MetricService) storeChainIdData(projectData map[string]map[string]*AggregatedMetric, data RelayMetrics, successCount int64) {
	chainIdData, exists := projectData[data.ChainID]
	if exists {
		m.storeApiTypeData(chainIdData, data, successCount)
	} else {
		chainIdData = map[string]*AggregatedMetric{
			data.APIType: {
				TotalLatency: data.Latency,
				RelaysCount:  1,
				SuccessCount: successCount,
			},
		}
		(*m.AggregatedMetricMap)[data.ProjectHash][data.ChainID] = chainIdData
	}
}

func (m *MetricService) storeApiTypeData(chainIdData map[string]*AggregatedMetric, data RelayMetrics, successCount int64) {
	apiTypesData, exists := chainIdData[data.APIType]
	if exists {
		apiTypesData.TotalLatency += data.Latency
		apiTypesData.SuccessCount += successCount
		apiTypesData.RelaysCount += 1
	} else {
		(*m.AggregatedMetricMap)[data.ProjectHash][data.ChainID][data.APIType] = &AggregatedMetric{
			TotalLatency: data.Latency,
			RelaysCount:  1,
			SuccessCount: successCount,
		}
	}
}
