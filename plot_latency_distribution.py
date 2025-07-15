import pandas as pd
import matplotlib.pyplot as plt

def plot_latency_distribution(csv_file):
    """
    Reads latency data from a CSV file and plots its distribution.

    Args:
        csv_file (str): The path to the CSV file.
    """
    df = pd.read_csv(csv_file)
    plt.figure(figsize=(10, 6))
    plt.hist(df['LatencySeconds'], bins=100, edgecolor='black')
    plt.title('RayCluster Ready Latency [Autopilot, no accelerators]')
    plt.xlabel('Latency (Seconds)')
    plt.ylabel('Frequency')
    plt.grid(True)
    plt.savefig('latency_distribution.png')
    print(f"Graph saved to latency_distribution.png")
    print(df.describe())

if __name__ == '__main__':
    plot_latency_distribution('benchmark_results.csv')
