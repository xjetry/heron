import { Fragment, useId, useRef, useState, type KeyboardEvent } from "react";
import { isDate, isTime } from "../lib/format";
import { Calendar } from "./Calendar";
import { Icon } from "./Icon";

// 日期、时刻、日期加时刻三种控件共用的分段输入：每段只收数字，填满自动跳到下一段，空段退格回上一段，
// 输入分隔符也跳段。外观与读法不随浏览器语言变（原生 date / time / datetime-local 在英文浏览器下是 mm/dd/yyyy 与 AM/PM）。
//
// 值的约定：全空是 ""；各段齐全（exact 段填满位数）时输出补零后的标准写法（YYYY-MM-DD、HH:MM、YYYY-MM-DDTHH:MM，
// 与原生控件的 value 同一格式）；否则把各段原样连起来输出，调用方据此知道"填了但没填完"，表单校验也据此拦下。
//
// 受控同步：只有 value 这个 prop 真的变了、且不是本控件刚发出的值时，才按它重置各段。所以调用方可以不接受未完成的值
// （日期筛选只把完整日期写进 URL），输入中的各段不会被旧的 prop 冲掉；外部改值（取消编辑、清除筛选）仍会同步进来。
type Segment = { name: string; length: number; placeholder: string; exact?: boolean; sep: string };
type Kind = { segments: Segment[]; valid: (value: string) => boolean; invalidText: string; calendar: boolean };

const DATE: Segment[] = [
  { name: "年", length: 4, placeholder: "YYYY", exact: true, sep: "" },
  { name: "月", length: 2, placeholder: "MM", sep: "-" },
  { name: "日", length: 2, placeholder: "DD", sep: "-" },
];
const TIME: Segment[] = [
  { name: "时", length: 2, placeholder: "HH", sep: "" },
  { name: "分", length: 2, placeholder: "MM", sep: ":" },
];
const KINDS = {
  date: { segments: DATE, valid: isDate, invalidText: "请输入四位年份和有效的完整日期", calendar: true },
  time: { segments: TIME, valid: isTime, invalidText: "请输入 00:00 到 23:59 之间的时刻", calendar: false },
  datetime: {
    segments: [...DATE, { ...TIME[0], sep: "T" }, TIME[1]],
    valid: (v: string) => { const [d, t = ""] = v.split("T"); return isDate(d) && isTime(t); },
    invalidText: "请输入有效的完整日期与 00:00 到 23:59 之间的时刻",
    calendar: true,
  },
} satisfies Record<string, Kind>;

const SEPARATOR_KEYS = new Set(["-", "/", ".", ":", " ", "T"]);

function partsOf(value: string, segments: Segment[]): string[] {
  const raw = value === "" ? [] : value.split(/[-T:]/);
  return segments.map((_, i) => raw[i] ?? "");
}

function compose(parts: string[], segments: Segment[]): string {
  if (parts.every((p) => p === "")) return "";
  const complete = parts.every((p, i) => p !== "" && (!segments[i].exact || p.length === segments[i].length));
  return parts.map((p, i) => segments[i].sep + (complete ? p.padStart(segments[i].length, "0") : p)).join("");
}

type Props = { label: string; caption?: string; value: string; onChange: (value: string) => void; required?: boolean; hint?: string; disabled?: boolean };

// label 是读屏名（各段读作「label 年」等），caption 是可见标题；表格里一行一个控件时 label 带上对象名，caption 只写字段名。
export function DateInput(props: Props) { return <SegmentedField kind={KINDS.date} {...props} />; }
export function TimeInput(props: Props) { return <SegmentedField kind={KINDS.time} {...props} />; }
export function DateTimeInput(props: Props) { return <SegmentedField kind={KINDS.datetime} {...props} />; }

function SegmentedField({ kind, label, caption, value, onChange, required = false, hint, disabled = false }: Props & { kind: Kind }) {
  const { segments } = kind;
  const [draft, setDraft] = useState(() => ({ emitted: value, parts: partsOf(value, segments) }));
  const [seen, setSeen] = useState(value);
  if (value !== seen) {
    setSeen(value);
    if (value !== draft.emitted) setDraft({ emitted: value, parts: partsOf(value, segments) });
  }
  const [open, setOpen] = useState(false);
  const inputs = useRef<(HTMLInputElement | null)[]>([]);
  const trigger = useRef<HTMLButtonElement>(null);
  const hintId = useId();
  // 校验看各段当前显示的内容而不是 prop：调用方不接受未完成的值时 prop 还是旧值，用户看到的却是没填完的段。
  const invalid = draft.emitted !== "" && !kind.valid(draft.emitted);
  const missing = required && draft.emitted === "";
  const message = invalid ? kind.invalidText : missing ? `请填写${caption ?? label}` : "";
  const emit = (parts: string[]) => {
    const next = compose(parts, segments);
    setDraft({ emitted: next, parts });
    onChange(next);
  };
  const focusSegment = (index: number) => { const el = inputs.current[index]; el?.focus(); el?.select(); };
  const change = (index: number, text: string) => {
    const digits = text.replace(/\D/g, "").slice(0, segments[index].length);
    emit(draft.parts.map((part, i) => (i === index ? digits : part)));
    if (digits.length === segments[index].length && index < segments.length - 1) focusSegment(index + 1);
  };
  const keyDown = (index: number, event: KeyboardEvent<HTMLInputElement>) => {
    const el = event.currentTarget;
    const atStart = el.selectionStart === 0 && el.selectionEnd === 0;
    const atEnd = el.selectionStart === el.value.length;
    if (event.key === "Backspace" && el.value === "" && index > 0) { event.preventDefault(); focusSegment(index - 1); }
    else if (event.key === "ArrowLeft" && atStart && index > 0) { event.preventDefault(); focusSegment(index - 1); }
    else if (event.key === "ArrowRight" && atEnd && index < segments.length - 1) { event.preventDefault(); focusSegment(index + 1); }
    else if (SEPARATOR_KEYS.has(event.key)) { event.preventDefault(); if (el.value !== "" && index < segments.length - 1) focusSegment(index + 1); }
  };
  const datePart = draft.parts.slice(0, 3).map((p, i) => (p === "" ? "" : p.padStart(DATE[i].length, "0"))).join("-");
  const pick = (ymd: string) => {
    emit([...ymd.split("-"), ...draft.parts.slice(3)]);
    setOpen(false);
    trigger.current?.focus();
  };
  return (
    <div className="date-field" role="group" aria-label={label}>
      {caption && <span className="field-caption">{caption}</span>}
      <div className="date-box" data-invalid={invalid || undefined} data-disabled={disabled || undefined}>
        {segments.map((segment, index) => (
          <Fragment key={segment.name}>
            {segment.sep && <span className="date-sep" aria-hidden="true">{segment.sep === "T" ? "\u00a0" : segment.sep}</span>}
            <input ref={(el) => { inputs.current[index] = el; el?.setCustomValidity(message); }}
              className="date-segment" data-length={segment.length} aria-label={`${label} ${segment.name}`}
              aria-describedby={invalid || hint ? hintId : undefined} aria-invalid={invalid || undefined}
              inputMode="numeric" autoComplete="off" maxLength={segment.length} placeholder={segment.placeholder}
              value={draft.parts[index]} disabled={disabled}
              onFocus={(event) => event.currentTarget.select()}
              onChange={(event) => change(index, event.target.value)}
              onKeyDown={(event) => keyDown(index, event)} />
          </Fragment>
        ))}
        {kind.calendar && <button ref={trigger} type="button" className="icon-button" aria-label={`选择${label}`} title="选择日期"
          aria-haspopup="dialog" aria-expanded={open} disabled={disabled} onClick={() => setOpen(!open)}><Icon name="calendar" /></button>}
        <button type="button" className="icon-button" aria-label={`清除${label}`} title="清除" disabled={disabled || draft.emitted === ""}
          onClick={() => { emit(segments.map(() => "")); focusSegment(0); }}><Icon name="close" /></button>
        {open && <Calendar label={label} value={datePart} onPick={pick} onClose={(returnFocus) => { setOpen(false); if (returnFocus) trigger.current?.focus(); }} />}
      </div>
      {(invalid || hint) && <small id={hintId} className={invalid ? "error" : "muted"}>{invalid ? kind.invalidText : hint}</small>}
    </div>
  );
}
