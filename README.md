# ElasticControllerGo: Autonomous Cloud Elasticity Controller

[![Go Version](https://img.shields.io/badge/Go-1.27.1+-00ADD8?style=flat&logo=go)](https://golang.org)
[![AWS SDK](https://img.shields.io/badge/AWS%20SDK%20v2-Go-FF9900?style=flat&logo=amazon-aws)](https://github.com/aws/aws-sdk-go-v2)

An autonomous horizontal auto-scaling controller and cloud evaluation engine implemented in Go using the **AWS SDK for Go v2**. 

`ElasticControllerGo` continuously monitors cluster telemetry via Amazon CloudWatch, calculates sliding-window workload gradients, and orchestrates horizontal scale-out and scale-in lifecycle actions across Amazon EC2 instances registered to an Application Load Balancer (ALB) Target Group.

---

## Table of Contents

- [System Architecture](#-system-architecture)
- [Key Features](#-key-features)
- [Instance Lifecycle & Safety Mechanisms](#-instance-lifecycle--safety-mechanisms)
- [Repository Structure](#-repository-structure)
- [Prerequisites & AWS Setup](#-prerequisites--aws-setup)
- [Configuration](#-configuration)
- [Build and Execution](#-build-and-execution)
- [Load Testing & Benchmarking](#-load-testing--benchmarking)
- [Experimental Results](#-experimental-results)

---

## System Architecture

The elasticity controller acts as an external control loop decoupling workload sensing, metric aggregation, and lifecycle orchestration from application containers:


---

## Key Features

- **Multi-Metric Telemetry Fusion**: Evaluates both compute pressure (`CPUUtilization`) and network bandwidth throughput (`NetworkIn`) using AWS CloudWatch batch queries (`GetMetricData`).
- **Trend-Aware Scaling**: Detects rate-of-change deltas ($\Delta \text{CPU}$) over sliding windows to respond to sudden traffic surges before saturation occurs.
- **Graceful Lifecycle Coordination**:
  - **Scale-Out**: Launches EC2 instance $\rightarrow$ Polls for `running` state $\rightarrow$ Registers target to ALB $\rightarrow$ Awaits target health status `healthy` before releasing cooldown.
  - **Scale-In**: Deregisters target from ALB $\rightarrow$ Awaits target drain state `unused` $\rightarrow$ Terminates EC2 instance $\rightarrow$ Awaits termination via AWS SDK waiters.
- **Flapping and Oscillation Protection**: Cooldown after a scaling operation is set to the lookback window (2 mins) so cloudwatch has time to gather metrics after the scaling
- **Capacity Bounds Enforcement**: Strict upper (`MaxInstances = 5`) and lower (`MinInstances = 1`) limits with fallback safeguard if all instances fail.
- **High-Throughput Synthetic Load Generator (`RealTrafficTest.go`)**: Multi-phase open-loop HTTP load generator measuring $P50$, $P90$, $P99$ latencies, HTTP status codes, and connection dynamics into structured CSV output.

---

### Threshold Parameters

| Metric / Parameter | Value | Description |
| :--- | :--- | :--- |
| **Minimum Capacity** (`minInstances`) | `1` | Guaranteed minimum cluster floor. |
| **Maximum Capacity** (`maxInstances`) | `5` | Maximum cluster ceiling to protect against runaway costs. |
| **Lookback Window** (`lookbackWindow`) | `2 minutes` | CloudWatch metric lookback and scaling cooldown window. |
| **Scale-Up CPU Threshold** | `> 70%` | Triggers immediate scale-out under heavy compute load. |
| **Rapid Surge CPU Threshold** | `> 50%` with $\Delta > 10\%$ | Proactively catches rapid traffic spikes. |
| **Scale-Up Network Threshold** | `> 20 MB/min` | Triggers scale-out when network bandwidth exceeds baseline. |
| **Scale-Down Dual Threshold** | `< 35%` CPU **AND** `< 5 MB/min` NetIn | Conservative scale-in to avoid premature termination. |

---

## Instance Lifecycle & Safety Mechanisms

### Safe Scale-Out Flow
1. **Launch**: Dispatches `ec2.RunInstances` with configured AMI, instance type (`t2.micro`), Security Group, and custom user-data script.
2. **Instance Readiness**: Invokes `WaitForInstanceRunning`, querying `DescribeInstanceStatus` until EC2 status is `running`.
3. **ALB Registration**: Calls `elasticloadbalancingv2.RegisterTargets` to add the instance ID to the Target Group.
4. **Health Check Convergence**: Invokes `WaitForTargetStateHealth`, polling `DescribeTargetHealth` until ALB reports `healthy`.
5. **Cooldown Activation**: Sets `lastScaleTime = time.Now()` to prevent duplicate scale events during metric lag.

### Graceful Scale-In Flow
1. **Target Selection**: Selects candidate instances for termination.
2. **Connection Draining (Deregistration)**: Calls `elasticloadbalancingv2.DeregisterTargets`.
3. **Drain Verification**: Awaits target state transitioning to `unused` (ALB deregistration delay completes and active in-flight requests finish).
4. **Termination**: Calls `ec2.TerminateInstances`.
5. **Termination Waiter**: Utilizes AWS SDK `ec2.NewInstanceTerminatedWaiter` to ensure instances enter terminal state cleanly.

---

## Repository Structure

```
ElasticControllerGo/
├── .agents/                    # Agent instructions and workflows
├── ControllerMain.go           # Core autonomous elasticity controller
├── RealTrafficTest.go          # High-performance multi-phase load generator
├── controller_scaling_results.png # Visual benchmark scaling graph
├── go.mod                      # Go module definition (Go 1.27.1)
├── go.sum                      # Checksums for AWS SDK v2 dependencies
├── TestResults/                # Performance benchmarks & experiment logs
│   ├── controllerTest3.log     # Controller execution trace
│   ├── experiment_metrics_Test1.xlsx
│   ├── experiment_metrics_Test2.xlsx
│   └── experiment_metrics_Test3.xlsx
├── AGENTS.md                   # Clean Architecture & SDK guidelines
└── README.md                   # Project documentation
```

---

## Prerequisites & AWS Setup

### 1. Go Runtime
Ensure you have **Go 1.22+** (configured with Go 1.27.1 module compatibility):
```powershell
go version
```

### 2. AWS Permissions
The execution identity requires IAM permissions for:
- **EC2**: `ec2:RunInstances`, `ec2:DescribeInstances`, `ec2:DescribeInstanceStatus`, `ec2:TerminateInstances`, `ec2:CreateTags`
- **ELBv2**: `elasticloadbalancing:RegisterTargets`, `elasticloadbalancing:DeregisterTargets`, `elasticloadbalancing:DescribeTargetHealth`
- **CloudWatch**: `cloudwatch:GetMetricData`

### 3. AWS Credentials
Configure your AWS credentials using the standard credential chain (If running locally and not on an EC2 instance):
```powershell
# Option A: AWS CLI
aws configure

# Option B: Environment Variables
$env:AWS_REGION="us-east-1"
$env:AWS_ACCESS_KEY_ID="<your-access-key>"
$env:AWS_SECRET_ACCESS_KEY="<your-secret-key>"
$env:AWS_SESSION_TOKEN="<your-session-token>" # if using AWS Academy / STS
```

---

## Build and Execution

### 1. Compile Controller and Load Generator

```powershell
# Build Controller
go build -o ControllerMain.exe ControllerMain.go

# Build Load Generator
go build -o realtraffic.exe RealTrafficTest.go
```

### 2. Run Elastic Controller

```powershell
.\ControllerMain.exe
```

The controller initializes AWS clients, opens `controller.log` (logging simultaneously to stdout and file via `io.MultiWriter`), and begins the evaluation loop:

```text
Loop #1 started
Found 1 matching instances
1: Instance i-0abcd1234efgh5678 of type t2.micro, has state: running and TG state is: healthy
Lookback window: 2m0s, Period: 1min, Metrics:
AVG CPU: 12.4  CPU change: 1.2
AVG Net In (MB): 1.15  Net In change: 0.05
MAINTAIN_CAPACITY, Reason: Metrics in steady state (CPU: 12.4%, NetIn: 1.15 MB/min)
```

---

## Load Testing & Benchmarking

The included `RealTrafficTest.go` generates realistic traffic workloads using an open-loop model calibrated to CloudWatch collection intervals and EC2 bootstrap times.

### Traffic Phases

| Phase | Duration | Target RPS | Objective |
| :--- | :--- | :--- | :--- |
| **1. Baseline / Warm-up** | 8 min | 100 RPS | Validates baseline performance with minimum capacity (1 instance). |
| **2. Sudden Demand Spike** | 10 min | 1500 RPS | Induces compute & network saturation to force scale-out to maximum capacity (5 instances). |
| **3. Sustained High** | 8 min | 500 RPS | Tests stability and prevents flapping under continuous load. |
| **4. Traffic Cooldown** | 10 min | 250 RPS | Induces low resource utilization to trigger graceful scale-in back to baseline. |

### Running the Load Test

```powershell
.\realtraffic.exe -url "http://<YOUR-ALB-DNS-NAME>" -out "experiment_metrics.csv"
```

Console status displays second-by-second throughput and latency percentiles:
```text
[14:05:01] Active:  150 | 2xx: 1498/s | 5xx:   0/s | Err:   0/s | Latency (p50:  12.4ms, p90:  28.1ms, p99:  54.2ms)
```

---

## Experimental Results

Experimental benchmarks demonstrate controller responsiveness under load:

![Scaling Results](controller_scaling_results.png)

1. **Surge Reaction**: During Phase 2 (1500 RPS), CPU utilization rose above 70%, triggering successive scale-out events up to the 5-instance cap. P99 latency dropped significantly as newly registered healthy instances absorbed traffic.
2. **Stabilization**: In Phase 3, instances remained steady without thrashing or false-positive deregistration.
3. **Graceful Draining**: During Phase 4, traffic subsided, allowing CPU and Network metrics to drop below scale-down thresholds. Targets were cleanly drained without connection resets (`5xx = 0`), returning the cluster safely to minimum capacity.

---

