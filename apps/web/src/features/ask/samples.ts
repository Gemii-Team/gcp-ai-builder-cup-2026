/**
 * The two sample questions, one in each language the Ask screen speaks. They are questions, not interface text: each
 * stays in its own language whichever language the interface is in, and the Thai one is tagged `lang="th"` where it is shown.
 * They match the seeded workspace (the Q4 launch and the ACME contract).
 */
export const SAMPLE_QUESTIONS = [
  { lang: 'en', text: "What did we decide about the Q4 launch date, and what's still open?" },
  { lang: 'th', text: 'สัญญา ACME หมดอายุเมื่อไหร่ และค่าปรับส่งช้าเท่าไร' },
] as const;
