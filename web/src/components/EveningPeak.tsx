import { useMemo, useState } from "react";
import { EVENING_PEAK_LABEL, eveningPeakCoverage, sliceEveningPeak, tryEveningPeakFit, type PeakCoverage, type PeakFit } from "../lib/eveningPeak";
import { InfoTip } from "./InfoTip";

export type ProbeSlice = "all" | "evening";

export type PeakSliceState = {
  slice: ProbeSlice;
  setSlice: (slice: ProbeSlice) => void;
  timeZone: string | undefined;
  // undefined：没有可用的 hub 时区（还没取到、取失败或 Intl 不认识），此时只能看全时段。
  fit: ((ts: number, stepS: number) => PeakFit) | undefined;
};

// timeZone 只能是 hub 下发的时区（GetSettings.timezone / GetSite.timezone）。没有它时不提供晚高峰，不退回访客的本地时区：
// 那样同一条线路的"晚间"会随访客所在地变成另一段时间。
export function usePeakSlice(timeZone: string | undefined): PeakSliceState {
  const [chosen, setSlice] = useState<ProbeSlice>("all");
  const fit = useMemo(() => tryEveningPeakFit(timeZone), [timeZone]);
  // 同一选择、同一时区下返回同一个对象：调用方把它放进 useMemo 的依赖，切片后的数据只在这两者变化时重算。
  const slice = fit ? chosen : "all";
  return useMemo(() => ({ slice, setSlice, timeZone, fit }), [slice, timeZone, fit]);
}

// 按当前选择切出要汇总与绘制的样本；全时段原样返回。coverage 只在晚高峰时有意义。
export function slicedSamples<T extends { ts: bigint }>(peak: PeakSliceState, samples: readonly T[], stepS: number): readonly T[] {
  return peak.slice === "evening" && peak.fit ? sliceEveningPeak(samples, stepS, peak.fit) : samples;
}

export function peakCoverage(peak: PeakSliceState, from: number, to: number, stepS: number): PeakCoverage | null {
  return peak.slice === "evening" && peak.fit && stepS > 0 ? eveningPeakCoverage(from, to, stepS, peak.fit) : null;
}

export function SliceButtons({ peak }: { peak: PeakSliceState }) {
  const { slice, setSlice, timeZone, fit } = peak;
  const options: { value: ProbeSlice; label: string }[] = [{ value: "all", label: "全时段" }, { value: "evening", label: "晚高峰" }];
  return (
    <>
      <nav aria-label="时段" className="segmented">
        {options.map((o) => {
          const unavailable = o.value === "evening" && !fit;
          return (
            <button key={o.value} type="button" onClick={() => setSlice(o.value)} aria-pressed={o.value === slice} disabled={unavailable}
              title={unavailable ? "取不到 Hub 时区，无法按晚高峰切片" : undefined}>
              {o.label}
            </button>
          );
        })}
      </nav>
      {fit && <InfoTip label="晚高峰说明">晚高峰是 Hub 时区（{timeZone}）每日 {EVENING_PEAK_LABEL}，不随访客所在时区变化。只计整个时间桶都落在其中的点；跨越边界的桶整桶不计。</InfoTip>}
    </>
  );
}

// covered 时不提示；另两种情形下图与汇总都没有可用的点，说清楚是步长太粗还是窗口里本来就没有晚间。
export function PeakNotice({ coverage, stepS }: { coverage: PeakCoverage | null; stepS: number }) {
  if (coverage === "step-too-large") return <p className="muted" role="status">当前每点 {stepS} 秒，没有整段落在晚高峰（{EVENING_PEAK_LABEL}）内的点；请缩短查询时间窗口。</p>;
  if (coverage === "outside-window") return <p className="muted" role="status">窗口内没有晚高峰时段（{EVENING_PEAK_LABEL}）。</p>;
  return null;
}
