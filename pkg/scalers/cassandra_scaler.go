package scalers

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gocql/gocql"
	v2beta2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/metrics/pkg/apis/external_metrics"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kedautil "github.com/kedacore/keda/v2/pkg/util"
)

// cassandraScaler exposes a data pointer to CassandraMetadata and gocql.Session connection.
type cassandraScaler struct {
	metadata *CassandraMetadata
	session  *gocql.Session
}

// CassandraMetadata defines metadata used by KEDA to query a Cassandra table.
type CassandraMetadata struct {
	username         string
	password         string
	clusterIPAddress string
	port             int
	consistency      gocql.Consistency
	protocolVersion  int
	keyspace         string
	query            string
	targetQueryValue int
	metricName       string
	scalerIndex      int
}

var cassandraLog = logf.Log.WithName("cassandra_scaler")

// NewCassandraScaler creates a new Cassandra scaler.
func NewCassandraScaler(config *ScalerConfig) (Scaler, error) {
	meta, err := ParseCassandraMetadata(config)
	if err != nil {
		return nil, fmt.Errorf("error parsing cassandra metadata: %s", err)
	}

	session, err := NewCassandraSession(meta)
	if err != nil {
		return nil, fmt.Errorf("error establishing cassandra session: %s", err)
	}

	return &cassandraScaler{
		metadata: meta,
		session:  session,
	}, nil
}

// ParseCassandraMetadata parses the metadata and returns a CassandraMetadata or an error if the ScalerConfig is invalid.
func ParseCassandraMetadata(config *ScalerConfig) (*CassandraMetadata, error) {
	meta := CassandraMetadata{}

	if val, ok := config.TriggerMetadata["query"]; ok {
		meta.query = val
	} else {
		return nil, fmt.Errorf("no query given")
	}

	if val, ok := config.TriggerMetadata["targetQueryValue"]; ok {
		targetQueryValue, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("targetQueryValue parsing error %s", err.Error())
		}
		meta.targetQueryValue = targetQueryValue
	} else {
		return nil, fmt.Errorf("no targetQueryValue given")
	}

	if val, ok := config.TriggerMetadata["username"]; ok {
		meta.username = val
	} else {
		return nil, fmt.Errorf("no username given")
	}

	if val, ok := config.TriggerMetadata["port"]; ok {
		port, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("port parsing error %s", err.Error())
		}
		meta.port = port
	}

	if val, ok := config.TriggerMetadata["clusterIPAddress"]; ok {
		switch p := meta.port; {
		case p > 0:
			meta.clusterIPAddress = fmt.Sprintf("%s:%d", val, meta.port)
		case strings.Contains(val, ":"):
			meta.clusterIPAddress = val
		default:
			return nil, fmt.Errorf("no port given")
		}
	} else {
		return nil, fmt.Errorf("no cluster IP address given")
	}

	if val, ok := config.TriggerMetadata["protocolVersion"]; ok {
		protocolVersion, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("protocolVersion parsing error %s", err.Error())
		}
		meta.protocolVersion = protocolVersion
	} else {
		meta.protocolVersion = 4
	}

	if val, ok := config.TriggerMetadata["consistency"]; ok {
		meta.consistency = gocql.ParseConsistency(val)
	} else {
		meta.consistency = gocql.One
	}

	if val, ok := config.TriggerMetadata["keyspace"]; ok {
		meta.keyspace = val
	} else {
		return nil, fmt.Errorf("no keyspace given")
	}

	if val, ok := config.TriggerMetadata["metricName"]; ok {
		meta.metricName = kedautil.NormalizeString(fmt.Sprintf("cassandra-%s", val))
	} else {
		meta.metricName = kedautil.NormalizeString(fmt.Sprintf("cassandra-%s", meta.keyspace))
	}

	if val, ok := config.AuthParams["password"]; ok {
		meta.password = val
	} else {
		return nil, fmt.Errorf("no password given")
	}

	meta.scalerIndex = config.ScalerIndex

	return &meta, nil
}

// NewCassandraSession returns a new Cassandra session for the provided CassandraMetadata.
func NewCassandraSession(meta *CassandraMetadata) (*gocql.Session, error) {
	cluster := gocql.NewCluster(meta.clusterIPAddress)
	cluster.ProtoVersion = meta.protocolVersion
	cluster.Consistency = meta.consistency
	cluster.Authenticator = gocql.PasswordAuthenticator{
		Username: meta.username,
		Password: meta.password,
	}

	session, err := cluster.CreateSession()
	if err != nil {
		cassandraLog.Error(err, "found error creating session")
		return nil, err
	}

	return session, nil
}

// IsActive returns true if there are pending events to be processed.
func (s *cassandraScaler) IsActive(ctx context.Context) (bool, error) {
	messages, err := s.GetQueryResult(ctx)
	if err != nil {
		return false, fmt.Errorf("error inspecting cassandra: %s", err)
	}

	return messages > 0, nil
}

// GetMetricSpecForScaling returns the MetricSpec for the Horizontal Pod Autoscaler.
func (s *cassandraScaler) GetMetricSpecForScaling(ctx context.Context) []v2beta2.MetricSpec {
	targetQueryValue := resource.NewQuantity(int64(s.metadata.targetQueryValue), resource.DecimalSI)
	externalMetric := &v2beta2.ExternalMetricSource{
		Metric: v2beta2.MetricIdentifier{
			Name: GenerateMetricNameWithIndex(s.metadata.scalerIndex, s.metadata.metricName),
		},
		Target: v2beta2.MetricTarget{
			Type:         v2beta2.AverageValueMetricType,
			AverageValue: targetQueryValue,
		},
	}
	metricSpec := v2beta2.MetricSpec{
		External: externalMetric, Type: externalMetricType,
	}

	return []v2beta2.MetricSpec{metricSpec}
}

// GetMetrics returns a value for a supported metric or an error if there is a problem getting the metric.
func (s *cassandraScaler) GetMetrics(ctx context.Context, metricName string, metricSelector labels.Selector) ([]external_metrics.ExternalMetricValue, error) {
	num, err := s.GetQueryResult(ctx)
	if err != nil {
		return []external_metrics.ExternalMetricValue{}, fmt.Errorf("error inspecting cassandra: %s", err)
	}

	metric := external_metrics.ExternalMetricValue{
		MetricName: metricName,
		Value:      *resource.NewQuantity(int64(num), resource.DecimalSI),
		Timestamp:  metav1.Now(),
	}

	return append([]external_metrics.ExternalMetricValue{}, metric), nil
}

// GetQueryResult returns the result of the scaler query.
func (s *cassandraScaler) GetQueryResult(ctx context.Context) (int, error) {
	var value int
	if err := s.session.Query(s.metadata.query).WithContext(ctx).Scan(&value); err != nil {
		if err != gocql.ErrNotFound {
			cassandraLog.Error(err, "query failed")
			return 0, err
		}
	}

	return value, nil
}

// Close closes the Cassandra session connection.
func (s *cassandraScaler) Close(ctx context.Context) error {
	s.session.Close()

	return nil
}
