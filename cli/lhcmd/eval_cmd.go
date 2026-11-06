package lhcmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yurika0211/luckyagent-app/evalplatform"
)

func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "冻结用例、离线判定并写出报告",
	}
	create := &cobra.Command{
		Use:   "create-run",
		Short: "冻结用例清单，不开始执行",
		RunE:  runEvalCreate,
	}
	create.Flags().String("dataset", "", "用例文件")
	create.Flags().String("out", "", "运行目录")
	_ = create.MarkFlagRequired("dataset")
	_ = create.MarkFlagRequired("out")
	start := &cobra.Command{
		Use:   "start",
		Short: "用进程内假执行器填写证据和判定",
		RunE:  runEvalStart,
	}
	start.Flags().String("run", "", "运行目录")
	_ = start.MarkFlagRequired("run")
	report := &cobra.Command{
		Use:   "report",
		Short: "根据已冻结的运行目录写出版本化报告",
		RunE:  runEvalReport,
	}
	report.Flags().String("run", "", "运行目录")
	report.Flags().String("out", "report.html", "HTML 报告路径")
	_ = report.MarkFlagRequired("run")
	cmd.AddCommand(create, start, report)
	return cmd
}

func runEvalCreate(cmd *cobra.Command, _ []string) error {
	dataset, _ := cmd.Flags().GetString("dataset")
	out, _ := cmd.Flags().GetString("out")
	run, err := evalplatform.CreateRun(dataset, out)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "created %s started=%t cases=%d\n", out, run.Started, len(run.Cases))
	return nil
}

func runEvalStart(cmd *cobra.Command, _ []string) error {
	dir, _ := cmd.Flags().GetString("run")
	run, err := evalplatform.Start(dir, evalplatform.SampleExecutor{})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "started %s cases=%d\n", dir, len(run.Cases))
	return nil
}

func runEvalReport(cmd *cobra.Command, _ []string) error {
	dir, _ := cmd.Flags().GetString("run")
	out, _ := cmd.Flags().GetString("out")
	if err := evalplatform.WriteReport(dir, out); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), out)
	return nil
}
