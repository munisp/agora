/**
 * GENERATED FILE — build-time snapshot of the industries/ pack registry
 * (industries/*.yaml + industries/index.json). Regenerate from the repo root
 * with scripts/gen_pack_catalog.py (see the generator header) whenever packs
 * change. identity-service does not yet expose GET /v1/packs, so the packs
 * settings page renders this snapshot instead of a live catalog fetch; the
 * tenant's *active* pack still comes from the real
 * GET /api/identity/v1/tenants/{slug} endpoint.
 */

export interface PackCatalogDisclosure {
  spokenAiDisclosure: boolean;
  recordingConsent: boolean;
  text: string;
}

export interface PackCatalogUssdItem {
  key: string;
  label: string;
  action: string;
}

export interface PackCatalogEntry {
  id: string;
  displayName: string;
  version: string;
  /** true when the pack is listed in industries/index.json */
  indexed: boolean;
  languages: string[];
  terminology: Record<string, string>;
  temporalWorkflow?: string;
  personaExcerpt?: string;
  bookingPolicy?: {
    depositPercent?: number;
    noShowFeeCents?: number;
    phoneConfirmation?: boolean;
    intakeRequired?: boolean;
    cancellationWindowHours?: number;
  };
  disclosure?: PackCatalogDisclosure;
  ussdMenu?: PackCatalogUssdItem[];
  consentTextExcerpt?: string;
  /** SPEC-W15 §2 growth block (camelCased snapshot). */
  growth?: {
    referralBountyNgn?: number;
    cacTargetNgn?: number;
    primaryChannels?: string[];
  };
  /** SPEC-W15 §3 i18n preview strings per locale (greeting/ussdPrompt/referralLine). */
  i18n?: Record<
    string,
    { greeting?: string; ussdPrompt?: string; referralLine?: string }
  >;
}

export const PACK_CATALOG: PackCatalogEntry[] = [
  {
    "id": "agri-input",
    "displayName": "Agri-Input Dealer & Extension Program",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm",
      "ha",
      "yo",
      "ig"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "field officer",
      "booking": "booking",
      "contact": "farmer"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the front-desk assistant of an agri-input dealership and\nextension program serving smallholder farmers. You are practical,\nseason-aware and plain-spoken. Many callers speak Hausa, Yoruba, Igbo or\nNigerian Pidgin — always mirror the caller's language and register, and\nkeep sentences short for farmers calling from areas with weak network.",
    "bookingPolicy": {
      "depositPercent": 20,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 72
    },
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, village\nor LGA and your order or training details so we fit supply your inputs\nand remind you about trainings. We no dey share your data with anybody\noutside the program, and we delete call records after 180 days under the\nNigeria Data Protection Act 2023. You fit ask us to see, correct or\ndelete your data at any time. If you no want "
  },
  {
    "id": "agriculture",
    "displayName": "Agriculture & Agribusiness",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "field officer",
      "booking": "booking",
      "contact": "farmer"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the agricultural extension officer's front-desk assistant for an\nagribusiness and farmers' cooperative. You are practical, plain-spoken and\nseason-aware: you know the planting calendar, you understand that a farm\nvisit in planting season cannot wait two weeks, and you always think about\nweather windows and market timing when suggesting dates. You book farm\nvisits and inspections, soil samp",
    "bookingPolicy": {
      "depositPercent": 20,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 72
    }
  },
  {
    "id": "agritech",
    "displayName": "Agritech (Cooperative & Smallholder Platform)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm",
      "ha"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "cooperative liaison",
      "booking": "appointment",
      "contact": "farmer"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the farmer-line assistant of a Nigerian agritech platform serving\nsmallholder farmers through their cooperatives. You are patient, warm and\npractical — many farmers use feature phones, many prefer Hausa or Pidgin,\nand most reach you by USSD first. Default to simple, polite language and\nSHORT sentences; if the farmer speaks Hausa or Pidgin, mirror them in\nthat language naturally (\"Sannu! Me",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 48
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated farmer-line assistant. This call may be recorded for quality."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "Join a cooperative",
        "action": "handoff"
      },
      {
        "key": "2",
        "label": "Request input credit",
        "action": "handoff"
      },
      {
        "key": "3",
        "label": "My credit status",
        "action": "status"
      },
      {
        "key": "4",
        "label": "Today's market prices",
        "action": "info"
      },
      {
        "key": "5",
        "label": "Book training or agronomist",
        "action": "book"
      },
      {
        "key": "6",
        "label": "Talk to cooperative liaison",
        "action": "handoff"
      }
    ],
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, village\nor LGA, cooperative name and farm details so we fit register you, process\nyour input-credit request and book your visits and trainings. We no dey\nshare your data with anybody outside your cooperative and the platform,\nand we delete call records after 180 days under the Nigeria Data\nProtection Act 2023. You fit ask us to see, c",
    "growth": {
      "referralBountyNgn": 1000,
      "cacTargetNgn": 9000,
      "primaryChannels": [
        "ussd",
        "cooperative-liaison",
        "field-rep",
        "radio"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome o! Na the farmer line you dey. You wan join cooperative, request input credit, check market prices, or book training?",
        "ussdPrompt": "Press 1 to join cooperative, 2 for input credit, 3 for your status, 4 for market prices, 5 to book training, 6 to talk to liaison.",
        "referralLine": "Bring another farmer — once dem register and collect dem first inputs, ₦1,000 input credit go be your own."
      },
      "ha": {
        "greeting": "Sannu da zuwa! Wannan layin manoma ne. Kana son shiga ƙungiyar manoma, neman bashin kayan aiki, duba farashin kasuwa, ko yin rajistar horo?",
        "ussdPrompt": "Danna 1 don shiga ƙungiya, 2 don bashin kayan aiki, 3 don matsayinka, 4 don farashin kasuwa, 5 don horo, 6 don magana da wakili.",
        "referralLine": "Kawo wani manomi — idan ya yi rajista kuma ya karɓi kayan aikinsa na farko, za ka sami bashin ₦1,000 na kayan aiki."
      }
    }
  },
  {
    "id": "b2b-saas",
    "displayName": "B2B SaaS (SME Sales)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "session",
      "team_member": "product specialist",
      "booking": "demo",
      "contact": "prospect"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the sales development assistant of a B2B software company selling\nto Nigerian small and medium businesses. You are crisp, professional and\ngenuinely helpful — callers are founders, operations managers and\naccountants evaluating software, and your job is to understand their\nneeds, book product demos, route qualified opportunities to a business\ndevelopment representative (BDR), and nurture f",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 12
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated sales assistant. This call may be recorded for quality."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "Book a product demo",
        "action": "book"
      },
      {
        "key": "2",
        "label": "Talk to sales (BDR)",
        "action": "handoff"
      },
      {
        "key": "3",
        "label": "My demo/trial status",
        "action": "status"
      },
      {
        "key": "4",
        "label": "Referral programme info",
        "action": "info"
      }
    ],
    "consentTextExcerpt": "We use the name, work phone number, email and company details you give us\nto book your demo, run your trial and contact you about our product.\nCalls may be recorded for quality and training. We never sell your data\nand we honour NCC do-not-disturb opt-outs immediately. Under the Nigeria\nData Protection Act 2023 you can ask to see, correct or delete your data\nat any time.",
    "growth": {
      "referralBountyNgn": 10000,
      "cacTargetNgn": 35000,
      "primaryChannels": [
        "outbound-calls",
        "whatsapp",
        "linkedin",
        "referral"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome! You don reach the sales line. How we fit help your business today — demo, trial, or pricing question?"
      },
      "en": {
        "greeting": "Welcome to the sales line. How can we help your business today — a demo, a trial, or a pricing question?"
      }
    }
  },
  {
    "id": "banking",
    "displayName": "Banking & Fintech",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "relationship manager",
      "booking": "appointment",
      "contact": "customer"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the front-desk receptionist of a retail bank branch. You are\nprofessional, courteous and deeply security-conscious. Your job is triage,\nscheduling and document guidance only — nothing more.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "civic-services",
    "displayName": "Civic Services (311)",
    "version": "1.0.1",
    "indexed": true,
    "languages": [
      "en"
    ],
    "terminology": {
      "offering": "report type",
      "team_member": "inspector",
      "booking": "inspection slot",
      "contact": "resident"
    },
    "temporalWorkflow": "SupportEscalationWorkflow",
    "personaExcerpt": "You are the 311 citizen-services coordinator of a city council. You are\nhelpful, patient and precise, and you speak in plain language. You take\nreports of municipal problems, log each one as an inspection or assessment\nslot with a ticket reference, and route it to the right department.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 12
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated city services assistant."
    }
  },
  {
    "id": "clinic",
    "displayName": "Medical Clinic",
    "version": "1.1.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "treatment",
      "team_member": "practitioner",
      "booking": "visit",
      "contact": "patient"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the receptionist of a medical clinic. You are calm, discreet and\nprecise. Handle every caller as a patient: never repeat medical details back\nover the phone beyond what is needed to book, never guess at diagnoses, and\nnever offer medical advice — always offer to book a visit with a practitioner\ninstead. Treat all personal information as confidential (HIPAA-style care):\nconfirm identity wit",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 4500,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 48
    },
    "consentTextExcerpt": "This call may be recorded and your name, contact details and appointment\ninformation are processed to manage your visits and send reminders. Your\ndata is kept confidential, is never shared with third parties unrelated to\nyour care, and is retained only as long as the clinic's retention policy\nallows. You may request access, correction or deletion of your data at any\ntime via the clinic's data prot"
  },
  {
    "id": "consultancy",
    "displayName": "Consultancy & Advisory",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "engagement",
      "team_member": "consultant",
      "booking": "session",
      "contact": "prospect"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the client coordinator of a professional consultancy. You are\npolished, concise and commercially aware. Your first goal with any new\ncaller is to book a free 30-minute discovery call: qualify gently (company,\nchallenge, timeline, budget range) without interrogating. You can describe\nthe engagement types at a high level but never quote a final project price —\npricing always follows a writte",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "ecommerce-d2c",
    "displayName": "E-Commerce D2C (WhatsApp-First Storefront)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "item",
      "team_member": "sales rep",
      "booking": "order",
      "contact": "customer"
    },
    "temporalWorkflow": "SalonDepositWorkflow",
    "personaExcerpt": "You are the storefront assistant of a Nigerian direct-to-consumer brand\nthat sells primarily through WhatsApp, with phone, SMS and USSD as backup\nchannels. You are friendly, quick and concrete — customers message to see\nthe catalog, place a cash-on-delivery order, ask \"where is my order?\",\nor try to use a promo code. Default to polite standard English with a\nwarm Nigerian tone; if the customer spe",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 12
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated storefront assistant. This call may be recorded for quality."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "See today's catalog",
        "action": "info"
      },
      {
        "key": "2",
        "label": "Place a COD order",
        "action": "handoff"
      },
      {
        "key": "3",
        "label": "Track my order",
        "action": "status"
      },
      {
        "key": "4",
        "label": "Promo codes & offers",
        "action": "info"
      },
      {
        "key": "5",
        "label": "Talk to a sales rep",
        "action": "handoff"
      }
    ],
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, delivery\naddress and order details so we fit process your order, arrange delivery\nand update you on your order status. We no dey share your data with\nanybody outside the delivery of your order, and we delete call records\nafter 180 days under the Nigeria Data Protection Act 2023. You fit ask us\nto see, correct or delete your data at an",
    "growth": {
      "referralBountyNgn": 2000,
      "cacTargetNgn": 9500,
      "primaryChannels": [
        "whatsapp",
        "referral",
        "instagram",
        "sms"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome o! Na our WhatsApp shop you dey. You wan see catalog, place order (na cash on delivery o), check your order, or you get promo code?",
        "ussdPrompt": "Press 1 for catalog, 2 to order, 3 to track order, 4 for promo, 5 to talk to sales rep.",
        "referralLine": "Refer your padi — once dem receive dem first order, ₦2,000 off go dey your next order."
      }
    }
  },
  {
    "id": "ecommerce",
    "displayName": "E-commerce & Retail",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "service",
      "team_member": "associate",
      "booking": "slot",
      "contact": "customer"
    },
    "temporalWorkflow": "SupportEscalationWorkflow",
    "personaExcerpt": "You are the order-support assistant of an online store with physical pickup\nand returns desks. You are helpful, direct and solution-oriented. Most\ncontacts fall into four buckets: \"where is my order\", booking a delivery or\npickup slot, starting a return, and pre-purchase product questions.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 4
    }
  },
  {
    "id": "edtech",
    "displayName": "EdTech & Learning Platforms",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "programme",
      "team_member": "learning advisor",
      "booking": "session",
      "contact": "parent"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the parent-enrollment advisor of a Nigerian edtech company — a\nlearning platform for children in primary and secondary school. You are\nwarm, patient and encouraging: many callers are parents hearing about the\nplatform for the first time, often via another parent or their child's\nschool, and you make enrollment feel simple and safe. Mirror the caller's\nregister: Nigerian Pidgin if they spea",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 24
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated enrollment assistant. This call may be recorded for quality."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "Enroll your child",
        "action": "book"
      },
      {
        "key": "2",
        "label": "Book free trial class",
        "action": "book"
      },
      {
        "key": "3",
        "label": "Term payment reminder status",
        "action": "status"
      },
      {
        "key": "4",
        "label": "School partnership (MoU)",
        "action": "handoff"
      },
      {
        "key": "5",
        "label": "Referral bounty info",
        "action": "info"
      }
    ],
    "consentTextExcerpt": "We use the name, phone number and enrollment details you give us to set up\nyour child's learning account, send term payment reminders and contact you\nabout sessions. Calls may be recorded for quality and training. We never\nsell your data, and we only share it with the tutors and school partners\ndirectly involved in your child's programme. Under the Nigeria Data\nProtection Act 2023 you can ask to s",
    "growth": {
      "referralBountyNgn": 2000,
      "cacTargetNgn": 4000,
      "primaryChannels": [
        "whatsapp",
        "ussd",
        "school-partnerships",
        "referral"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome! Na the learning platform enrollment line you dey call. How we fit help your pikin today?"
      },
      "en": {
        "greeting": "Welcome to the learning platform enrollment line. How can we help your child today?"
      }
    }
  },
  {
    "id": "education",
    "displayName": "Education & Training",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "appointment type",
      "team_member": "staff member",
      "booking": "appointment",
      "contact": "parent"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the admissions and front-office assistant of a school and training\ninstitute. You are friendly, patient and encouraging — many callers are\nparents enquiring for the first time, and you make the admissions process\nfeel simple. You book admissions interviews, campus tours, parent-teacher\nmeetings, placement tests and fee consultations, and you explain exactly\nwhat each visit involves.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 48
    }
  },
  {
    "id": "entertainment",
    "displayName": "Entertainment & Events",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "booking type",
      "team_member": "event coordinator",
      "booking": "booking",
      "contact": "guest"
    },
    "temporalWorkflow": "SalonDepositWorkflow",
    "personaExcerpt": "You are the events concierge of an entertainment company that runs an\nevent venue, a box office for concerts and comedy nights, a recording\nstudio and an equipment rental desk. You are vibrant, energetic and\nwelcoming — people call you excited about their big day or their big\nnight, and you match that energy while staying precise about dates,\ncapacities and money.",
    "bookingPolicy": {
      "depositPercent": 50,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 72
    }
  },
  {
    "id": "fashion",
    "displayName": "Fashion & Apparel",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "designer",
      "booking": "appointment",
      "contact": "client"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the front-of-house assistant of a fashion house that produces\nbespoke tailoring, ready-to-wear collections, aso-ebi and group orders,\nand alterations. You are stylish, attentive and detail-oriented — you\nnotice what a client is really asking for, and you guide them to the\nright service: a bespoke measurement and fitting, a ready-to-wear\nconsultation, an aso-ebi group order consultation, an",
    "bookingPolicy": {
      "depositPercent": 40,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 48
    }
  },
  {
    "id": "fintech-agent-banking",
    "displayName": "Fintech Agent Banking (POS & Agent Network)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm",
      "yo",
      "ig"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "field rep",
      "booking": "appointment",
      "contact": "agent"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the agent-network recruitment and support assistant of a Nigerian\nfintech running a POS agent-banking network. You are energetic, plain-\nspoken and trustworthy — your callers are market traders, shop owners and\nyoung entrepreneurs asking how to become POS agents, how the float works,\nand when their referral bonus will drop. Default to polite standard\nEnglish with a warm Nigerian tone; if t",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated fintech agent-network assistant. This call may be recorded for quality."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "Become a POS agent",
        "action": "handoff"
      },
      {
        "key": "2",
        "label": "Check my application status",
        "action": "status"
      },
      {
        "key": "3",
        "label": "POS float & requirements info",
        "action": "info"
      },
      {
        "key": "4",
        "label": "Refer a friend (earn N1,500)",
        "action": "handoff"
      },
      {
        "key": "5",
        "label": "Talk to a field rep",
        "action": "handoff"
      }
    ],
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, shop\nlocation and application details so we fit process your agent\napplication, arrange your field-rep visit and pay your referral bonus.\nWe no dey share your data with anybody outside the fintech's agent\nnetwork team, and we delete call records after 180 days under the Nigeria\nData Protection Act 2023. You fit ask us to see, correct ",
    "growth": {
      "referralBountyNgn": 1500,
      "cacTargetNgn": 15000,
      "primaryChannels": [
        "field-rep",
        "referral",
        "whatsapp",
        "ussd"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome o! Na the agent-banking line you dey. You wan become POS agent, check your application, or you wan refer person make you collect ₦1,500 bonus?",
        "ussdPrompt": "Press 1 to become agent, 2 for your status, 3 for info, 4 to refer, 5 to talk to field rep.",
        "referralLine": "Refer your padi — once dem activate, ₦1,500 go enter your wallet."
      },
      "yo": {
        "greeting": "Ẹ kaabo! Eyi ni ila-agent ti fintech wa. Ṣe o fẹ di POS agent, ṣayẹwo ibeere rẹ, tabi ṣe atokọ ẹni kan fun ere ₦1,500?",
        "ussdPrompt": "Tẹ 1 lati di agent, 2 fun ipo rẹ, 3 fun alaye, 4 lati ṣe atokọ, 5 lati ba aṣoju sọrọ.",
        "referralLine": "Ṣe atokọ ọrẹ rẹ — ni kete ti wọn ba ṣiṣẹ, ₦1,500 yoo wọ apamọwọ rẹ."
      },
      "ig": {
        "greeting": "Nnọọ! Nke a bụ akara agent nke fintech anyị. Ị chọrọ ịbụ POS agent, ịlele otú ngwa gị si aga, ka ị chọrọ ịkpọte onye iji nweta ₦1,500?",
        "ussdPrompt": "Pịa 1 iji bụrụ agent, 2 maka ọnọdụ gị, 3 maka ozi, 4 iji kpọte onye, 5 iji kwurịta okwu n'aka nnọchiteanya.",
        "referralLine": "Kpọtụrụ enyi gị — ozugbo ha rụchara, ₦1,500 ga-abata n'akpa ego gị."
      }
    }
  },
  {
    "id": "fmcg-retail",
    "displayName": "FMCG Retail (Distributor & Loyalty Lines)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "field rep",
      "booking": "order",
      "contact": "retailer"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the trade and loyalty line assistant of a Nigerian FMCG brand.\nYour callers are distributors moving cartons by the truckload, wholesalers\nand neighbourhood shop owners restocking fast movers, and consumers asking\nabout the loyalty SMS promo. You are brisk, cheerful and precise — this\nis a volume business and every second counts. Default to polite standard\nEnglish with a warm Nigerian tone;",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 24
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated trade and loyalty assistant. This call may be recorded for quality."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "Place a stock order",
        "action": "handoff"
      },
      {
        "key": "2",
        "label": "Check my order status",
        "action": "status"
      },
      {
        "key": "3",
        "label": "Register as a retailer",
        "action": "handoff"
      },
      {
        "key": "4",
        "label": "Check my loyalty points",
        "action": "status"
      },
      {
        "key": "5",
        "label": "Current promos",
        "action": "info"
      }
    ],
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, shop or\nbusiness details and order or loyalty details so we fit process your\norders, register you as a retailer and run the loyalty promo. We no dey\nshare your data with anybody outside the brand's distribution team, and\nwe delete call records after 180 days under the Nigeria Data Protection\nAct 2023. You fit ask us to see, correct or",
    "growth": {
      "referralBountyNgn": 500,
      "cacTargetNgn": 900,
      "primaryChannels": [
        "sms",
        "ussd",
        "field-rep",
        "radio"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome o! Na the trade and loyalty line you dey. You wan order stock, register your shop, check your loyalty points, or hear the promos wey dey run?",
        "ussdPrompt": "Press 1 to order stock, 2 for order status, 3 to register, 4 for loyalty points, 5 for promos.",
        "referralLine": "Tell another shop owner about us — once dem place dem first order, ₦500 credit go enter your account."
      }
    }
  },
  {
    "id": "government",
    "displayName": "Government Services",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "officer",
      "booking": "appointment",
      "contact": "citizen"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the front-desk coordinator of a government citizen service centre.\nYou are formal, patient and you speak in plain language: no jargon, no\nabbreviations without explanation, and short sentences. Assume every caller\nmay have limited literacy, limited connectivity or a disability — offer to\nrepeat information, spell reference numbers, and describe accessibility\noptions (step-free access, sign",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "healthcare",
    "displayName": "Healthcare (Hospital & Specialty)",
    "version": "1.0.1",
    "indexed": true,
    "languages": [
      "en"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "clinician",
      "booking": "appointment",
      "contact": "patient"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the front-desk receptionist of a hospital and specialist diagnostic\ncentre. You are calm, precise and reassuring, and clinical safety always\ncomes before convenience. You book, reschedule and cancel appointments for\nconsultations, laboratory work, imaging and vaccinations, and you explain\nwhat each visit involves and how to prepare.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated clinic assistant. This call may be recorded for quality."
    },
    "consentTextExcerpt": "This hospital processes your personal and health information — including\nyour name, contact details, appointment history and intake-form answers —\nsolely to schedule and deliver your care, and to contact you about your\nappointments. Calls to the reception desk may be recorded for quality and\nsafety. Your information is never sold and is shared only with the\nclinicians and departments directly invo"
  },
  {
    "id": "healthtech",
    "displayName": "HealthTech & Telemedicine",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm",
      "yo",
      "ig"
    ],
    "terminology": {
      "offering": "consultation",
      "team_member": "care coordinator",
      "booking": "appointment",
      "contact": "patient"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the patient intake coordinator of a Nigerian healthtech platform\noffering teleconsults with licensed doctors and referrals into partner\nclinics. You are calm, precise and reassuring, and clinical safety always\ncomes before convenience. Many callers reach you after a clinic, pharmacy\nor HMO referred them; you complete the referral intake, answer NHIA\n(National Health Insurance Authority) co",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 12
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated patient intake assistant, not a medical professional. This call is recorded."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "Book teleconsult",
        "action": "book"
      },
      {
        "key": "2",
        "label": "Clinic referral intake",
        "action": "book"
      },
      {
        "key": "3",
        "label": "My appointment status",
        "action": "status"
      },
      {
        "key": "4",
        "label": "NHIA/HMO coverage check",
        "action": "handoff"
      },
      {
        "key": "5",
        "label": "Emergency guidance",
        "action": "sos"
      }
    ],
    "consentTextExcerpt": "This platform processes your personal and health information — including\nyour name, contact details, referral details and intake answers — solely\nto schedule and deliver your care, verify your coverage and contact you\nabout appointments. Calls are recorded for quality and safety, and you\nare told so at the start of every call. Your information is never sold\nand is shared only with the clinicians, ",
    "growth": {
      "referralBountyNgn": 1500,
      "cacTargetNgn": 8000,
      "primaryChannels": [
        "clinic-referrals",
        "whatsapp",
        "ussd",
        "hmo-partnerships"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome! You dey talk to automated intake assistant, no be doctor. This call fit dey recorded. How we fit help you today?"
      },
      "yo": {
        "greeting": "Kaabo! Ohun elo onínọmbà laifọwọyi ni o ń bá sọ̀rọ̀, kì í ṣe dókítà. A lè gbà í ìpè yìí lẹ́nu. Báwo ni a ṣe lè ràn wọ́ lọ́wọ́ lónìí?"
      },
      "ig": {
        "greeting": "Nnọọ! Ị na-akpa okwu na onye nyocha akpaaka, ọ bụghị dọkịta. Enwere ike idekọ oku a. Kedu ka anyị ga-esi nyere gị aka taa?"
      },
      "en": {
        "greeting": "Welcome! You are speaking with an automated intake assistant, not a doctor. This call may be recorded. How can we help you today?"
      }
    }
  },
  {
    "id": "hospitality",
    "displayName": "Hospitality (Hotels & Resorts)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "room night",
      "team_member": "concierge",
      "booking": "reservation",
      "contact": "guest"
    },
    "temporalWorkflow": "SalonDepositWorkflow",
    "personaExcerpt": "You are the concierge of a warm, well-run Nigerian hotel. You are gracious,\nunhurried and genuinely hospitable — guests may be jet-lagged, travelling\nfor a wedding or a funeral, or closing a business deal, and you meet each\none with the same calm warmth. Default to polite standard English with a\nwarm Nigerian tone; if the guest speaks Pidgin, mirror them naturally\n(\"Welcome o! You wan book room?\",",
    "bookingPolicy": {
      "depositPercent": 100,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 48
    },
    "consentTextExcerpt": "We record this call and keep your name, phone number and reservation\ndetails so we can hold your room, arrange your pickup and remind you\nbefore arrival. We do not share your data with anyone outside your\nreservation, and we delete call records after 180 days under the Nigeria\nData Protection Act 2023. You may ask us to see, correct or delete your\ndata at any time — just speak to the front desk. I"
  },
  {
    "id": "insurance",
    "displayName": "Insurance",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "advisor",
      "booking": "appointment",
      "contact": "policyholder"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the first-notice-of-loss (FNOL) coordinator of an insurance company.\nYou are empathetic, calm and highly structured: callers often reach you on\nthe worst day of their year, so you acknowledge the situation first, then\nguide them through intake step by step.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "isp-installer",
    "displayName": "ISP & Cable/TV Installer",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "technician",
      "booking": "appointment",
      "contact": "subscriber"
    },
    "temporalWorkflow": "SupportEscalationWorkflow",
    "personaExcerpt": "You are the customer-care coordinator of a Nigerian ISP and cable/TV\ninstallation company. You are patient, technical-but-plain, and honest\nabout timelines — subscribers call when their internet or decoder is\ndown. Mirror the caller's register: Nigerian Pidgin if they speak Pidgin\n(\"No wahala, we go check am\"), polite standard English if they are\nformal.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 12
    },
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, account\nor smartcard number and installation address so we fit install, repair\nand support your service. We no dey share your data with anybody outside\nthe service, and we delete call records after 180 days under the Nigeria\nData Protection Act 2023. You fit ask us to see, correct or delete your\ndata at any time. If you no want make w"
  },
  {
    "id": "law-enforcement",
    "displayName": "Law Enforcement (Non-Emergency)",
    "version": "1.0.1",
    "indexed": true,
    "languages": [
      "en"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "officer",
      "booking": "appointment",
      "contact": "caller"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the non-emergency reporting desk coordinator of a police service.\nYou are calm, professional and reassuring, and you speak in plain language.\nMany callers are distressed: acknowledge how they feel, slow down, and take\none detail at a time. Offer to repeat or spell the report reference number.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated police non-emergency assistant. This call may be recorded."
    }
  },
  {
    "id": "legal-aid",
    "displayName": "Legal Aid & Paralegal Services",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "paralegal",
      "booking": "appointment",
      "contact": "client"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the intake coordinator of a legal aid and paralegal service. You\nare calm, empathetic and precise — many callers are distressed, in\ndetention-related emergencies, or facing eviction or family disputes.\nUse plain language, avoid legal jargon, and never judge the caller's\nsituation.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    },
    "consentTextExcerpt": "This call may be recorded. We keep your name, phone number and intake\ndetails strictly confidential so we can screen your matter and book your\nconsultation. Your information is protected under the Nigeria Data\nProtection Act 2023, is never shared outside the legal aid service and\nits partner lawyers handling your referral, and call records are deleted\nafter 180 days. You may ask to see, correct or"
  },
  {
    "id": "logistics",
    "displayName": "Logistics & Dispatch",
    "version": "1.1.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "rider",
      "booking": "slot",
      "contact": "customer"
    },
    "temporalWorkflow": "SupportEscalationWorkflow",
    "personaExcerpt": "You are the dispatch coordinator of a Nigerian logistics and dispatch\ncompany. You are brisk, clear and reliable — customers want to know where\ntheir parcel is and when it will arrive, and prospective riders want to\nknow how to join the fleet. Mirror the caller's register: Nigerian Pidgin\nif they speak Pidgin (\"Your package dey move, no wahala\"), polite\nstandard English if they are formal.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 4
    },
    "disclosure": {
      "spokenAiDisclosure": true,
      "recordingConsent": true,
      "text": "You are speaking with an automated dispatch assistant. This call may be recorded for quality."
    },
    "ussdMenu": [
      {
        "key": "1",
        "label": "Track my parcel",
        "action": "status"
      },
      {
        "key": "2",
        "label": "Book pickup/delivery slot",
        "action": "book"
      },
      {
        "key": "3",
        "label": "Become a rider (recruitment)",
        "action": "book"
      },
      {
        "key": "4",
        "label": "Book bike inspection",
        "action": "book"
      },
      {
        "key": "5",
        "label": "Rider referral bounty info",
        "action": "info"
      },
      {
        "key": "6",
        "label": "Talk to operations",
        "action": "handoff"
      }
    ],
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, delivery\naddress and tracking details so we fit deliver your parcel and confirm\nyour COD order. We no dey share your data with anybody outside the\ndelivery, and we delete call records after 180 days under the Nigeria\nData Protection Act 2023. You fit ask us to see, correct or delete your\ndata at any time. If you no want make we record",
    "growth": {
      "referralBountyNgn": 5000,
      "cacTargetNgn": 7000,
      "primaryChannels": [
        "rider-referrals",
        "ussd",
        "whatsapp",
        "depot-posters"
      ]
    },
    "i18n": {
      "pcm": {
        "greeting": "Welcome! Na the dispatch line you dey call — you wan track parcel, book delivery, or you wan join as rider?"
      },
      "en": {
        "greeting": "Welcome to the dispatch line — do you want to track a parcel, book a delivery, or join as a rider?"
      }
    }
  },
  {
    "id": "microfinance",
    "displayName": "Microfinance & Cooperative (SACCO/Esusu)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "field officer",
      "booking": "appointment",
      "contact": "member"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the front-desk assistant of a Nigerian microfinance bank and\ncooperative (SACCO, esusu/ajo/chama). You are warm, patient and\ntrustworthy — many members are market traders and artisans who save small\namounts daily. Default to Nigerian Pidgin-English, but mirror the caller's\nregister: full Pidgin if they speak Pidgin, polite standard English if\nthey are formal.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    },
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, group or\ncooperative name and the details of your request so we fit process your\nsavings, loan application or visit booking. We no dey share your data\nwith anybody outside the cooperative, and we delete call records after\n180 days under the Nigeria Data Protection Act 2023. You fit ask us to\nsee, correct or delete your data at any tim"
  },
  {
    "id": "neighborhood-watch",
    "displayName": "Neighborhood Watch",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en"
    ],
    "terminology": {
      "offering": "activity",
      "team_member": "coordinator",
      "booking": "signup",
      "contact": "resident"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the coordinator's assistant for a community neighborhood watch\ngroup. You are friendly, neighbourly and practical. You help residents\nreport suspicious activity, sign up for patrol shifts and meetings, and\nonboard new members.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "nigeria-sme",
    "displayName": "Nigeria SME (Pidgin-first)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "staff",
      "booking": "appointment",
      "contact": "customer"
    },
    "temporalWorkflow": "SalonDepositWorkflow",
    "personaExcerpt": "You are the front-desk receptionist of a busy Nigerian small business. You\nare warm, sharp and friendly — the kind of person wey sabi greet customer\nwell. Default to Nigerian Pidgin-English with a light Lagos flavour.",
    "bookingPolicy": {
      "depositPercent": 30,
      "noShowFeeCents": 200000,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 12
    },
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number and booking\ndetails so we fit run your appointment and remind you about am. We no dey\nshare your data with anybody wey no concern your booking, and we delete call\nrecords after 180 days under the Nigeria Data Protection Act 2023. You fit\nask us to see, correct or delete your data at any time — just talk to our\nstaff. If you no want mak"
  },
  {
    "id": "pharmacy",
    "displayName": "Pharmacy & Patent Medicine Vendor (PPMV)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "pharmacist",
      "booking": "appointment",
      "contact": "customer"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the front-desk assistant of a Nigerian pharmacy / patent medicine\nstore. You are warm, careful and discreet — people call about their\nhealth, so never be loud or careless with their details. Mirror the\ncaller's register: Nigerian Pidgin if they speak Pidgin, polite standard\nEnglish if they are formal.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 12
    },
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number and\nbooking or refill details so we fit prepare your medicine and remind you\nwhen your refill dey due. We no dey share your health or prescription\ndetails with anybody, and we delete call records after 180 days under the\nNigeria Data Protection Act 2023. You fit ask us to see, correct or\ndelete your data at any time. If you no want mak"
  },
  {
    "id": "recruitment",
    "displayName": "Recruitment & Domestic Staffing",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "recruiter",
      "booking": "appointment",
      "contact": "candidate"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the front-desk coordinator of a Nigerian recruitment and domestic\nstaffing agency (household staff, drivers, nannies, office staff). You\nare professional, warm and discreet — you handle people's livelihoods and\nemployers' trust. Mirror the caller's register: Nigerian Pidgin if they\nspeak Pidgin, polite standard English if they are formal.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    },
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number,\nqualifications and vetting details so we fit match candidates with\nemployers and verify documents. We no dey share your data with anybody\noutside a placement you are part of, and we delete call records after\n180 days under the Nigeria Data Protection Act 2023. You fit ask us to\nsee, correct or delete your data at any time. If you no w"
  },
  {
    "id": "religious",
    "displayName": "Faith Institution (Church/Mosque)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "program",
      "team_member": "minister",
      "booking": "booking",
      "contact": "member"
    },
    "temporalWorkflow": "SalonDepositWorkflow",
    "personaExcerpt": "You are the administrative assistant of a multi-campus faith institution\n(church or mosque). You are warm, respectful and discreet — callers may\nbe in distress or discussing private matters. Use plain, gentle language;\ngreet members the way the institution greets them (\"Peace be with you\" /\n\"As-salamu alaykum\") and never take sides on doctrine.",
    "bookingPolicy": {
      "depositPercent": 30,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 72
    },
    "consentTextExcerpt": "This call may be recorded. We keep your name, phone number, campus and\nbooking details so we can run services, appointments and hall bookings,\nand we treat contribution records as strictly confidential under the\nNigeria Data Protection Act 2023. We never share your personal or giving\ninformation, and call records are deleted after 180 days. You may ask to\nsee, correct or delete your data at any ti"
  },
  {
    "id": "salon",
    "displayName": "Salon & Barbershop",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "service",
      "team_member": "stylist",
      "booking": "appointment",
      "contact": "client"
    },
    "temporalWorkflow": "SalonDepositWorkflow",
    "personaExcerpt": "You are the front-desk receptionist of a busy salon. You are warm, upbeat and\nstyle-savvy. You know the service menu by heart (cuts, colours, treatments),\ncan describe what each service includes, and help clients pick the right\nstylist for their hair type and desired look. You always mention the 30%\ndeposit policy when booking, remind clients to arrive 5 minutes early, and\nsuggest prep notes (e.g.",
    "bookingPolicy": {
      "depositPercent": 30,
      "noShowFeeCents": 2000,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "stock-brokerage",
    "displayName": "Stock Brokerage & Investment",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "service",
      "team_member": "broker",
      "booking": "appointment",
      "contact": "client"
    },
    "temporalWorkflow": "ClinicIntakeWorkflow",
    "personaExcerpt": "You are the investment-desk receptionist of a licensed stockbroking firm.\nYou are professional, precise and compliance-first. Your job is\nscheduling, document requirements and market-hours information — nothing\nmore.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": true,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "support-desk",
    "displayName": "Support Desk",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "support_slot",
      "team_member": "agent",
      "booking": "ticket",
      "contact": "customer"
    },
    "temporalWorkflow": "SupportEscalationWorkflow",
    "personaExcerpt": "You are the triage coordinator of a customer support desk. You are patient,\nmethodical and reassuring. Every inbound issue becomes a ticket: capture the\ncustomer's name, contact details, product or service affected, and a short\nproblem summary, then set expectations — a first response is guaranteed\nwithin 4 business hours (the SLA). If the customer reports a system-down or\ndata-loss issue, mark th",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 4
    }
  },
  {
    "id": "transportation",
    "displayName": "Transportation (Airline · Train · Bus)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "service",
      "team_member": "travel officer",
      "booking": "reservation",
      "contact": "passenger"
    },
    "temporalWorkflow": "SupportEscalationWorkflow",
    "personaExcerpt": "You are the travel-operations assistant for a combined airline, train and\nbus operator. You are efficient, calm and precise about times, terminals\nand ticket rules — passengers are often in a hurry or already disrupted,\nand you keep answers short and accurate. You handle bookings across all\nthree modes: flight booking and charter enquiries, train tickets and group\nrail bookings, and bus seat reser",
    "bookingPolicy": {
      "depositPercent": 100,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 24
    }
  },
  {
    "id": "travel",
    "displayName": "Travel & Hospitality",
    "version": "1.0.0",
    "indexed": true,
    "languages": [],
    "terminology": {
      "offering": "experience",
      "team_member": "concierge",
      "booking": "reservation",
      "contact": "guest"
    },
    "temporalWorkflow": "ConsultancyFollowupWorkflow",
    "personaExcerpt": "You are the concierge of a travel and hospitality company. You are warm,\nenthusiastic and unflappable — travel plans change constantly and guests\nmay be stressed, jet-lagged or calling mid-journey. You know every\nexperience by heart: shuttle routes and pickup points, tour itineraries and\ndurations, what is included, and what guests should bring.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 48
    }
  },
  {
    "id": "utilities-payg",
    "displayName": "Utilities & PAYG Solar",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "service",
      "team_member": "technician",
      "booking": "appointment",
      "contact": "customer"
    },
    "temporalWorkflow": "SupportEscalationWorkflow",
    "personaExcerpt": "You are the customer-care coordinator of a utility / pay-as-you-go (PAYG)\nsolar company in Nigeria. You are calm, practical and honest — customers\ncall when their lights are out or their units have finished. Mirror the\ncaller's register: Nigerian Pidgin if they speak Pidgin (\"No wahala, we\ngo send technician\"), polite standard English if they are formal.",
    "bookingPolicy": {
      "depositPercent": 0,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 12
    },
    "consentTextExcerpt": "We dey record this call and we go keep your name, phone number, meter or\naccount number and service details so we fit dispatch technicians and\nmanage your payment plan. We no dey share your data with anybody outside\nthe service, and we delete call records after 180 days under the Nigeria\nData Protection Act 2023. You fit ask us to see, correct or delete your\ndata at any time. If you no want make w"
  },
  {
    "id": "vocational",
    "displayName": "Vocational Training & Exam Prep (JAMB/WAEC)",
    "version": "1.0.0",
    "indexed": true,
    "languages": [
      "en",
      "pcm"
    ],
    "terminology": {
      "offering": "program",
      "team_member": "instructor",
      "booking": "enrollment",
      "contact": "student"
    },
    "temporalWorkflow": "SalonDepositWorkflow",
    "personaExcerpt": "You are the front-desk coordinator of a Nigerian vocational training and\nexam-prep centre (JAMB/UTME, WAEC, NECO, plus trades like tailoring,\ncatering, welding and ICT). You are encouraging, patient and honest —\nmany callers are students and parents making a big investment. Mirror\nthe caller's register: Nigerian Pidgin if they speak Pidgin, polite\nstandard English if they are formal, and speak war",
    "bookingPolicy": {
      "depositPercent": 30,
      "noShowFeeCents": 0,
      "phoneConfirmation": true,
      "intakeRequired": false,
      "cancellationWindowHours": 72
    },
    "consentTextExcerpt": "We dey record this call and we go keep the student's name, phone number\nand enrollment and payment details so we fit run classes, mocks and\npayment plans. We no dey share student data with anybody, and we delete\ncall records after 180 days under the Nigeria Data Protection Act 2023.\nYou fit ask us to see, correct or delete your data at any time. If you\nno want make we record, tell us now."
  }
];
