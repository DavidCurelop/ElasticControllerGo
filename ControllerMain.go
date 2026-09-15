package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
)

type InstanceService struct {
	ec2Client *ec2.Client
}

type MetricService struct {
	cloudwatchClient *cloudwatch.Client
}

type elbService struct {
	elbClient *elasticloadbalancingv2.Client
}

type MetricRecord struct {
	ID    string
	Value float64
}

func findMetricValue(records []cwtypes.MetricDataResult, targetID string, lookback int) (float64, time.Time, bool) {
	if len(records) == 0 || lookback < 0 {
		return 0.0, time.Time{}, false
	}
	for _, record := range records {
		if aws.ToString(record.Id) == targetID {
			if lookback >= len(record.Values) || lookback >= len(record.Timestamps){
				return 0.0, time.Time{}, false
			}
			return record.Values[lookback], record.Timestamps[lookback], true
		}
	}
	return 0.0, time.Time{}, false
}

func main() {

	// TODO: Step 1 - Load config via config.LoadDefaultConfig(ctx)
	ctx := context.Background()
	//awsConfig, configError := config.LoadDefaultConfig(ctx)
	awsConfig, configError := config.LoadDefaultConfig(ctx)
	tgARN := "arn:aws:elasticloadbalancing:us-east-1:046172315547:targetgroup/WebServerTG/60e063ee1bef4ce2"
	instanceAMI := "ami-0f8a61b66d1accaee"
	instanceTag := "TGTest"
	// TODO: Step 2 - Check err; if err != nil, exit with log.Fatalf(...)
	if configError != nil {
		log.Fatalf("AWS config loading failed with error %v", configError)
	}

	// Service creation and instantiation
	ec2Client := ec2.NewFromConfig(awsConfig)
	instanceService := &InstanceService{
		ec2Client: ec2Client,
	}

	elbClient := elasticloadbalancingv2.NewFromConfig(awsConfig)

	elbService := &elbService{
		elbClient: elbClient,
	}

	cwClient := cloudwatch.NewFromConfig(awsConfig)

	cwService := &MetricService{
		cloudwatchClient: cwClient,
	}

	// TODO: Step 5 - Call instanceService.LaunchInstance(...) and print the instance ID!
	for i := 0; i < 0; i++ {
		instanceID, err := instanceService.LaunchInstance(ctx, instanceAMI, types.InstanceTypeT2Micro, instanceTag)

		if err != nil {
			log.Fatalf("failed to launch instance %s", err)
		}

		log.Printf("Launched instance: %s", instanceID)
	}

	//time.Sleep(3 * time.Minute)

	instances, err := instanceService.GetInstancesByTag(ctx, "Name", instanceTag)
	log.Printf("Found %d matching instances", len(instances))

	if err != nil {
		log.Fatalf("Error retrieving instances %s", err)
	}

	for i, instance := range instances {
		fmt.Printf("%v: Instance %v of type %v and has state %v\n", i, aws.ToString(instance.InstanceId), instance.InstanceType, *aws.String(string(instance.State.Name)))

		if instance.State.Name == "running" {
			elberr := elbService.RegisterTarget(ctx, tgARN, instance)
			if elberr != nil {
				log.Fatalf("Error assigning %v to TG: %s\n", aws.ToString(instance.InstanceId), elberr)
			}
			fmt.Printf("Successfully added %v to TG\n", aws.ToString(instance.InstanceId))
			results, err := cwService.GetInstanceMetrics(ctx, *instance.InstanceId, 2*time.Minute)
			if err != nil {
				log.Fatalf("Error getting metrics: %v", err)
			}
			cpu, cpuMeasureTime, _ := findMetricValue(results, "cpuQuery", 0)
			net, netMeasureTime, _ := findMetricValue(results, "networkInQuery", 0)

			fmt.Printf("AVG CPU at %v: %v || Network In at %v: %v\n", cpuMeasureTime, cpu, netMeasureTime, net)

			cpu1, cpuMeasureTime1, _ := findMetricValue(results, "cpuQuery", 1)
			net1, netMeasureTime1, _ := findMetricValue(results, "networkInQuery", 1)

			fmt.Printf("AVG CPU at %v: %v || Network In at %v: %v\n", cpuMeasureTime1, cpu1, netMeasureTime1, net1)
		}
	}

	//time.Sleep(60 * time.Second)

	//instanceService.TerminateInstances(ctx, instances, -1)
}

func (s *InstanceService) LaunchInstance(ctx context.Context, imageID string, instanceType types.InstanceType, instanceTag string) (string, error) {
	// TODO: Step 1 - Construct &ec2.RunInstancesInput with explicit, multi-line named fields:
	//       - ImageId (use aws.String)
	//       - InstanceType (the types.InstanceType passed in)
	//       - MinCount & MaxCount (use aws.Int32)

	runInstanceInput := &ec2.RunInstancesInput{
		ImageId:      &imageID,
		InstanceType: instanceType,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
		Monitoring: &types.RunInstancesMonitoringEnabled{Enabled: aws.Bool(true)},
		TagSpecifications: []types.TagSpecification{
			{
				ResourceType: types.ResourceTypeInstance,
				Tags:         []types.Tag{{Key: aws.String("Name"), Value: &instanceTag}},
			},
		},
	}

	// TODO: Step 2 - Call s.ec2Client.RunInstances(ctx, runInstancesInput)

	runInstancesOutput, err := s.ec2Client.RunInstances(ctx, runInstanceInput)

	// TODO: Step 3 - Check error immediately with early return ("Line of sight").
	//       Wrap the error using: fmt.Errorf("launching EC2 instance with AMI %q: %w", imageID, err)
	if err != nil {
		return "", fmt.Errorf("launching EC2 instance with AMI %q: %w", imageID, err)
	}

	if len(runInstancesOutput.Instances) == 0 {
		return "", fmt.Errorf("no %q instance created", imageID)
	}

	// TODO: Step 4 - Inspect the returned reservation output to extract and return the instance ID string.
	return aws.ToString(runInstancesOutput.Instances[0].InstanceId), nil
}

func (s *InstanceService) GetInstancesByTag(ctx context.Context, tagName string, tagValue string) ([]types.Instance, error) {
	// TODO: Step 1 - Construct &ec2.DescribeInstancesInput with a types.Filter ("tag:" + tagName)
	describeInstancesInput := &ec2.DescribeInstancesInput{
		Filters: []types.Filter{{
			Name: aws.String("tag:" + tagName),
			Values: []string{
				*aws.String(tagValue),
			},
		}},
	}
	// TODO: Step 2 - Call s.ec2Client.DescribeInstances(ctx, describeInstancesInput)
	describeInstanceOutput, err := s.ec2Client.DescribeInstances(ctx, describeInstancesInput)
	// TODO: Step 3 - Handle error with early exit and storytelling fmt.Errorf wrapping
	if err != nil {
		return nil, fmt.Errorf("Something went wrong while looking for instances %s", err)
	}
	// TODO: Step 4 - Initialize a slice: var instanceIDs []string
	var instances []types.Instance
	// TODO: Step 5 - Range over Reservations and Instances, appending aws.ToString(inst.InstanceId)
	for _, reservtion := range describeInstanceOutput.Reservations {
		for _, instance := range reservtion.Instances {
			instances = append(instances, instance)
		}
	}
	// TODO: Step 6 - Return instanceIDs and nil
	return instances, nil
}

func (s *InstanceService) TerminateInstances(ctx context.Context, instances []types.Instance, amount int) (*ec2.TerminateInstancesOutput, error) {
	var instancesID []string
	if amount > len(instances) || amount < 0 {
		amount = len(instances)
	}

	for i := 0; i < amount; i++ {
		instance := instances[i]
		instancesID = append(instancesID, *instance.InstanceId)
	}
	terminateInput := &ec2.TerminateInstancesInput{
		InstanceIds: instancesID,
	}
	TerminateOutput, err := s.ec2Client.TerminateInstances(ctx, terminateInput)

	if err != nil {
		return nil, fmt.Errorf("%s", err)
	}

	for _, terminatedInstance := range TerminateOutput.TerminatingInstances {
		fmt.Printf("Succesfully terminated Instance: %s Status: %s\n", *aws.String(*terminatedInstance.InstanceId), terminatedInstance.CurrentState.Name)
	}

	return TerminateOutput, nil
}

func (s *MetricService) GetInstanceMetrics(ctx context.Context, instanceID string, lookbackWindow time.Duration) ([]cwtypes.MetricDataResult, error) {
	queryEndTime := time.Now()
	queryStartTime := queryEndTime.Add(-lookbackWindow)
	// TODO: Step 1 - Construct &cloudwatch.GetMetricDataInput with explicit multi-line fields:
	//       - StartTime: aws.Time(queryStartTime)
	//       - EndTime:   aws.Time(queryEndTime)
	//       - MetricDataQueries: []types.MetricDataQuery{ ... }
	//         Each query needs:
	//           - Id: (a unique query identifier string, e.g., "cpuQuery")
	//           - MetricStat: Metric (Namespace, MetricName, Dimensions), Period, Stat
	getmetricsInput := cloudwatch.GetMetricDataInput{
		StartTime: aws.Time(queryStartTime),
		EndTime:   aws.Time(queryEndTime),
		ScanBy:    cwtypes.ScanByTimestampDescending,
		MetricDataQueries: []cwtypes.MetricDataQuery{
			{Id: aws.String("cpuQuery"),
				MetricStat: &cwtypes.MetricStat{
					Period: aws.Int32(60),
					Stat:   aws.String("Average"),
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/EC2"),
						MetricName: aws.String("CPUUtilization"),
						Dimensions: []cwtypes.Dimension{{
							Name:  aws.String("InstanceId"),
							Value: aws.String(instanceID)},
						},
					},
				}},
			{Id: aws.String("networkInQuery"),
				MetricStat: &cwtypes.MetricStat{
					Period: aws.Int32(60),
					Stat:   aws.String("Average"),
					Metric: &cwtypes.Metric{
						Namespace:  aws.String("AWS/EC2"),
						MetricName: aws.String("NetworkIn"),
						Dimensions: []cwtypes.Dimension{{
							Name:  aws.String("InstanceId"),
							Value: aws.String(instanceID)},
						},
					},
				}},
		},
	}

	// TODO: Step 2 - Call s.cloudwatchClient.GetMetricData(ctx, metricDataInput)
	getmetricsOutput, err := s.cloudwatchClient.GetMetricData(ctx, &getmetricsInput)
	// TODO: Step 3 - Apply "Line of sight" error check and storytelling wrapping:
	//       fmt.Errorf("querying CloudWatch CPU metrics for instance %q: %w", instanceID, err)
	if err != nil {
		return nil, fmt.Errorf("querying CloudWatch metrics for instance %q: %w", instanceID, err)
	}
	// TODO: Step 4 - Inspect the returned results. Handle the "no data yet" case safely.
	//       Return the most recent data point value, or a sentinel/zero value if empty.
	return getmetricsOutput.MetricDataResults, nil
}

func (s *elbService) RegisterTarget(ctx context.Context, targetGroupARN string, instance types.Instance) error {
	// TODO: Step 1 - Build &elasticloadbalancingv2.RegisterTargetsInput
	registerTargetInput := &elasticloadbalancingv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(targetGroupARN),
		Targets: []elbtypes.TargetDescription{{
			Id:               aws.String(*instance.InstanceId),
			AvailabilityZone: nil,
		}},
	}
	// TODO: Step 2 - Call s.elbClient.RegisterTargets(ctx, registerTargetsInput)
	_, err := s.elbClient.RegisterTargets(ctx, registerTargetInput)
	// TODO: Step 3 - Line of sight error handling with storytelling wrapping:
	//       fmt.Errorf("registering instance %q to target group %q: %w", instanceID, targetGroupARN, err)
	if err != nil {
		return fmt.Errorf("registering instance %q to target group %q: %w", *instance.InstanceId, targetGroupARN, err)
	}
	return err
}
