import type { UiLabels } from './en';

// Thai. A Latin word inside a Thai string is marked `<en>word</en>` so it renders `lang="en"`; the Thai around it is
// tagged `lang="th"` by the renderer. REF codes and counted numbers are passed in as parameters and stay literal.
export const th: UiLabels = {
  // Proof slip
  generated: 'สร้างเมื่อ {time}',
  sources: 'แหล่งที่มา',
  draftsToConfirm: { one: '{count} ฉบับร่างรอยืนยัน', other: '{count} ฉบับร่างรอยืนยัน' },
  noDraftsWaiting: 'ไม่มีฉบับร่างที่รออยู่',
  decision: 'ข้อตัดสินใจ',
  openQuestion: 'คำถามที่ค้างอยู่',

  // Failed
  failed: 'ล้มเหลวเมื่อ {time}',
  retry: 'ลองอีกครั้ง',

  // Pending
  answerProgress: 'ความคืบหน้าของคำตอบ',
  retrieving: { one: 'กำลังค้นหาจาก {count} แชนเนล…', other: 'กำลังค้นหาจาก {count} แชนเนล…' },
  retrieved: { one: 'ค้นหาจาก {count} แชนเนลแล้ว', other: 'ค้นหาจาก {count} แชนเนลแล้ว' },
  writingAnswer: 'เขียนคำตอบ',
  writingAnswerNow: 'กำลังเขียนคำตอบ…',

  // Offset, page and message links; citation markers
  transcriptAt: 'บทถอดเสียงที่ {time}',
  pageShort: 'หน้า {page}',
  pageOf: '{file} หน้า {page}',
  messageIn: 'ข้อความใน #{channel} เวลา {time}',
  messageInUntimed: 'ข้อความใน #{channel}',
  sourceN: 'แหล่งที่มา {n}',
  sourceNOf: 'แหล่งที่มา {n}: {label}',

  // Transcript excerpt
  close: 'ปิด',
  excerptLoading: 'กำลังโหลดข้อความตัดตอน…',
  excerptError: 'โหลดข้อความตัดตอนนี้ไม่ได้',
  transcriptExcerptAt: 'ข้อความตัดตอนจากบทถอดเสียงที่ {time}',
  transcriptExcerpt: 'ข้อความตัดตอนจากบทถอดเสียง',
  documentExcerpt: 'ข้อความตัดตอนจากเอกสาร {file} หน้า {page}',
  documentExcerptNoPage: 'ข้อความตัดตอนจากเอกสาร {file}',
  messageExcerpt: 'ข้อความตัดตอนจากข้อความของ {author}',
  transcriptHeader: 'บทถอดเสียง · {title} · {time}',
  transcriptHeaderUntimed: 'บทถอดเสียง · {title}',
  messageHeader: 'ข้อความ · #{channel} · {day} {time}',
  messageHeaderToday: 'ข้อความ · #{channel} · {time}',
  documentHeader: 'เอกสาร · {file} · หน้า {page}',
  documentPage: 'หน้า {page} จาก {pages} · {ref}',
  documentPageNoCount: 'หน้า {page} · {ref}',

  // Seal
  confirmed: 'ยืนยันแล้ว',
  confirmedStamp: 'ยืนยันแล้ว',
};
