package admin

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/twmb/kafka-go/pkg/kerr"
	"github.com/twmb/kafka-go/pkg/kmsg"

	"github.com/twmb/kcl/client"
	"github.com/twmb/kcl/flagutil"
	"github.com/twmb/kcl/out"
)

func logdirsDescribeCommand(cl *client.Client) *cobra.Command {
	var broker int32
	cmd := &cobra.Command{
		Use:   "log-dirs",
		Short: "Describe log directories for topic partitions.",
		Long: `Describe log directories for topic partitions (Kafka 1.0.0+).

Log directories are partition specific. The size of a directory is the absolute
size of log segments of a partition, in bytes.

Offset lag is how far behind the log end offset is compared to the partition's
high watermark, or, if this dir is a "future" directory, how far behind
compared to the current replica's log end offset.

In math,

  OffsetLag = isFuture
              ? localLogEndOffset - futureLogEndOffset
              : max(localHighWaterMark - logEndOffset, 0)


A directory is a "future" directory if it was created with an alter command and
will replace the replica's current log directory in the future.

Input format is topic:1,2,3.

Alternatively, if you just specify a topic, this will describe all partitions
for that topic.

By default, this command will return log dirs for the partition leaders.

If describing everything, this will merge all in sync replicas into the same
response.

You can direct this request to specific brokers with the --broker argument,
which allows you to control whether you are asking for information about
replicas vs. the leader.
`,

		Example: `log-dirs foo:1,2,3 bar:3,4,5

log-dirs foo

log-dirs // describes all`,

		Run: func(_ *cobra.Command, topics []string) {
			var req kmsg.DescribeLogDirsRequest
			if topics != nil {
				tps, err := flagutil.ParseTopicPartitions(topics)
				out.MaybeDie(err, "improper topic partitions format on: %v", err)

				// For any topic that had no partitions specified, we
				// describe all partitions. We have to first learn of
				// these partitions with a metadata request.
				var alls []string
				for topic, partitions := range tps {
					if len(partitions) == 0 {
						alls = append(alls, topic)
					}
				}
				if len(alls) > 0 {
					meta := new(kmsg.MetadataRequest)
					for _, topic := range alls {
						meta.Topics = append(meta.Topics, kmsg.MetadataRequestTopic{Topic: topic})
					}
					kresp, err := cl.Client().Request(context.Background(), meta)
					out.MaybeDie(err, "unable to request metadata to determine partitions on topics: %v", err)
					resp := kresp.(*kmsg.MetadataResponse)
					for _, topic := range resp.Topics {
						for _, partition := range topic.Partitions {
							tps[topic.Topic] = append(tps[topic.Topic], partition.Partition)
						}
					}
				}

				for topic, partitions := range tps {
					req.Topics = append(req.Topics, kmsg.DescribeLogDirsRequestTopic{
						Topic:      topic,
						Partitions: partitions,
					})
				}
			}

			var kresp kmsg.Response
			var err error
			if broker >= 0 {
				kresp, err = cl.Client().Broker(int(broker)).Request(context.Background(), &req)
			} else {
				kresp, err = cl.Client().Request(context.Background(), &req)
			}
			out.MaybeDie(err, "unable to describe log dirs: %v", err)
			if cl.AsJSON() {
				out.ExitJSON(kresp)
			}
			resp := kresp.(*kmsg.DescribeLogDirsResponse)

			sort.Slice(resp.Dirs, func(i, j int) bool { return resp.Dirs[i].Dir < resp.Dirs[j].Dir })
			for _, dir := range resp.Dirs {
				sort.Slice(dir.Topics, func(i, j int) bool { return dir.Topics[i].Topic < dir.Topics[j].Topic })
				for _, topic := range dir.Topics {
					sort.Slice(topic.Partitions, func(i, j int) bool { return topic.Partitions[i].Partition < topic.Partitions[j].Partition })
				}
			}

			tw := out.BeginTabWrite()
			defer tw.Flush()

			fmt.Fprintf(tw, "DIR\tDIR ERR\tTOPIC\tPARTITION\tSIZE\tOFFSET LAG\tIS FUTURE\n")
			for _, dir := range resp.Dirs {
				if err := kerr.ErrorForCode(dir.ErrorCode); err != nil {
					fmt.Fprintf(tw, "%s\t%s\t\t\t\t\t\n", dir.Dir, err.Error())
					continue
				}
				for _, topic := range dir.Topics {
					for _, partition := range topic.Partitions {
						fmt.Fprintf(tw, "%s\t\t%s\t%d\t%d\t%d\t%v\n",
							dir.Dir,
							topic.Topic,
							partition.Partition,
							partition.Size,
							partition.OffsetLag,
							partition.IsFuture,
						)
					}
				}
			}

		},
	}

	cmd.Flags().Int32VarP(&broker, "broker", "b", -1, "a specific broker to direct the request to")
	return cmd
}

func logdirsAlterReplicasCommand(cl *client.Client) *cobra.Command {
	var broker int32
	cmd := &cobra.Command{
		Use:   "replica-log-dirs",
		Short: "Move topic replicas to a destination directory",
		Long: `Move topic partitions to specified directories (Kafka 1.0.0+).

Introduced in Kafka 1.0.0, this command allows for moving replica log
directories. See KIP-113 for the motivation.

The input syntax is topic:1,2,3=/destination/directory.

By default, this command will alter log dirs for the partition leaders.
You can direct this request to specific brokers with the --broker argument,
which allows you to alter replicas.
`,

		Example: `replica-log-dirs foo:1,2,3=/dir bar:6=/dir2 baz:9=/dir`,

		Run: func(_ *cobra.Command, topics []string) {
			dests := make(map[string]map[string][]int32)
			for _, topic := range topics {
				parts := strings.Split(topic, "=")
				if len(parts) != 2 {
					out.Die("improper format for dest-dir = split (expected two strings after split, got %d)", len(parts))
				}
				tps, err := flagutil.ParseTopicPartitions([]string{parts[0]})
				out.MaybeDie(err, "improper topic partitions format on %q: %v", parts[0], err)
				dest := parts[1]
				existing := dests[dest]
				if existing == nil {
					dests[dest] = make(map[string][]int32)
				}
				for topic, parts := range tps {
					dests[dest][topic] = append(dests[dest][topic], parts...)
				}
			}

			var req kmsg.AlterReplicaLogDirsRequest
			for dest, tps := range dests {
				reqDest := kmsg.AlterReplicaLogDirsRequestDir{
					Dir: dest,
				}
				for topic, partitions := range tps {
					reqDest.Topics = append(reqDest.Topics, kmsg.AlterReplicaLogDirsRequestDirTopic{
						Topic:      topic,
						Partitions: partitions,
					})
				}
				req.Dirs = append(req.Dirs, reqDest)
			}

			var kresp kmsg.Response
			var err error
			if broker >= 0 {
				kresp, err = cl.Client().Broker(int(broker)).Request(context.Background(), &req)
			} else {
				kresp, err = cl.Client().Request(context.Background(), &req)
			}
			out.MaybeDie(err, "unable to alter replica log dirs: %v", err)
			if cl.AsJSON() {
				out.ExitJSON(kresp)
			}

			resp := kresp.(*kmsg.AlterReplicaLogDirsResponse)
			tw := out.BeginTabWrite()
			defer tw.Flush()

			fmt.Fprintf(tw, "TOPIC\tPARTITION\tERROR\n")
			for _, topic := range resp.Topics {
				for _, partition := range topic.Partitions {
					msg := ""
					if err := kerr.ErrorForCode(partition.ErrorCode); err != nil {
						msg = err.Error()
					}
					fmt.Fprintf(tw, "%s\t%d\t%s\n",
						topic.Topic,
						partition.Partition,
						msg,
					)
				}
			}
		},
	}
	cmd.Flags().Int32VarP(&broker, "broker", "b", -1, "a specific broker to direct the request to")
	return cmd
}
