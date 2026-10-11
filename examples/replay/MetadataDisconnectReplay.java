import java.util.*;
import org.apache.kafka.clients.Metadata;
import org.apache.kafka.clients.MockClient;
import org.apache.kafka.clients.consumer.OffsetResetStrategy;
import org.apache.kafka.clients.consumer.internals.ConsumerNetworkClient;
import org.apache.kafka.clients.consumer.internals.Fetcher;
import org.apache.kafka.clients.consumer.internals.SubscriptionState;
import org.apache.kafka.common.*;
import org.apache.kafka.common.metrics.Metrics;
import org.apache.kafka.common.protocol.Errors;
import org.apache.kafka.common.requests.MetadataResponse;
import org.apache.kafka.common.serialization.ByteArrayDeserializer;
import org.apache.kafka.common.utils.SystemTime;
import org.apache.kafka.common.utils.Time;

/** Exercises unmodified historical Kafka client classes with the shipped MockClient. */
public class MetadataDisconnectReplay {
    private static final String TOPIC = "esa-replay-topic";
    private static void run(boolean disconnected) {
        Time time = new SystemTime();
        MockClient network = new MockClient(time);
        Node node = new Node(0, "localhost", 9092);
        PartitionInfo partition = new PartitionInfo(TOPIC, 0, node, new Node[]{node}, new Node[]{node});
        Cluster cluster = new Cluster(Collections.singleton(node), Collections.singleton(partition), Collections.<String>emptySet());
        Metadata metadata = new Metadata(0, Long.MAX_VALUE);
        metadata.update(cluster, time.milliseconds());
        network.setNode(node);
        Metrics metrics = new Metrics(time);
        ConsumerNetworkClient client = new ConsumerNetworkClient(network, metadata, time, 10);
        Fetcher<byte[],byte[]> fetcher = new Fetcher<>(client, 1, 0, 1000, true,
                new ByteArrayDeserializer(), new ByteArrayDeserializer(), metadata,
                new SubscriptionState(OffsetResetStrategy.EARLIEST), metrics,
                "esa-replay", Collections.<String,String>emptyMap(), time, 10);
        try {
            if (disconnected) {
                for (int i=0;i<1000;i++) network.prepareResponse(null, true);
            } else {
                network.prepareResponse(new MetadataResponse(cluster, Collections.<String,Errors>emptyMap()).toStruct());
            }
            Map<String,List<PartitionInfo>> result = fetcher.getTopicMetadata(Collections.singletonList(TOPIC), 100);
            if (!disconnected && (!result.containsKey(TOPIC) || result.get(TOPIC).size()!=1))
                throw new AssertionError("Successful metadata control returned unexpected partitions");
            System.out.println(disconnected ? "DISCONNECTED_RETURN="+result : "SUCCESSFUL_METADATA_CONTROL=PASS");
        } finally { metrics.close(); }
    }
    public static void main(String[] args) {
        run(false);
        try { run(true); throw new AssertionError("Disconnected lookup returned without a timeout"); }
        catch (org.apache.kafka.common.errors.TimeoutException expected) {
            System.out.println("DISCONNECTED_METADATA_RESULT=TimeoutException");
            System.out.println("FIXED_REGRESSION=PASS");
        } catch (Throwable failure) {
            System.out.println("DISCONNECTED_METADATA_RESULT="+failure.getClass().getName());
            failure.printStackTrace(System.out);
            System.exit(20);
        }
    }
}
